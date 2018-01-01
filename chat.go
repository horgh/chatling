package main

import (
	"fmt"
	"log"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	ircclient "github.com/horgh/boxcat"
	"github.com/horgh/irc"
)

// WebClient represents a websockets client
type WebClient struct {
	verbose    bool
	remoteAddr string
	conn       *websocket.Conn
}

func (h Handler) chatRequest(w http.ResponseWriter, r *http.Request) {
	conn, err := h.upgrader.Upgrade(w, r, nil)
	if err != nil {
		h.logError(r, fmt.Errorf("error upgrading to websocket connection: %s",
			err))
	}
	defer func() {
		if err := conn.Close(); err != nil {
			h.logError(r, fmt.Errorf("error closing websocket connection: %s", err))
		}
		if h.verbose {
			log.Printf("%s: closed websocket", r.RemoteAddr)
		}
	}()
	if h.verbose {
		log.Printf("%s: opened websocket", r.RemoteAddr)
	}
	wc := WebClient{h.verbose, r.RemoteAddr, conn}

	// The first message should provide info we need to set up/locate the
	// connection.
	m, err := readWebSocket(conn)
	if err != nil {
		h.logError(r, err)
		return
	}
	name, ok := m["name"]
	if !ok || name == "" {
		h.logError(r, fmt.Errorf("no name specified"))
		return
	}
	// TODO(horgh): Password

	recvChan, sendChan, err := h.getIRCConnection(name)
	if err != nil {
		h.logError(r, fmt.Errorf("error retrieving IRC connection: %s", err))
		return
	}

	webRecvChan := make(chan map[string]string, 64)
	go wc.webSocketReader(webRecvChan)

LOOP:
	for {
		select {
		case m, ok := <-webRecvChan:
			if !ok {
				break LOOP
			}
			wc.handleClientMessage(sendChan, m)
		case m, ok := <-recvChan:
			if !ok {
				break LOOP
			}
			wc.handleIRCMessage(m)
		}
	}

	// We could remove the listener from the IRCClient's listeners. It will be
	// cleaned up when it blocks though.
}

func (w *WebClient) logError(err error) {
	log.Printf("%s: %s", w.remoteAddr, err)
}

// Client holds an IRC client connection and provides access to it from
// multiple goroutines.
//
// An *ircclient.Client is only usable by a single goroutine. Primarily because
// it publishes each message only once. In order to handle multiple web clients
// all using the same one, we wrap around it here and deliver the message to
// each.
type Client struct {
	name      string
	client    *ircclient.Client
	listeners []chan<- irc.Message
	mutex     *sync.Mutex
}

func (h Handler) getIRCConnection(name string) (
	<-chan irc.Message,
	chan<- irc.Message,
	error,
) {
	name = strings.ToLower(name)
	recvChan := make(chan irc.Message, 128)

	h.clientsMutex.Lock()
	defer h.clientsMutex.Unlock()

	client, ok := h.clients[name]
	if ok {
		client.mutex.Lock()
		defer client.mutex.Unlock()
		client.listeners = append(client.listeners, recvChan)
		return recvChan, client.client.GetSendChannel(), nil
	}

	c := ircclient.NewClient(name, h.ircServer, h.ircPort)
	_, sendChan, _, err := c.Start()
	if err != nil {
		return nil, nil, fmt.Errorf("error starting IRC client: %s: %s", name, err)
	}

	client = &Client{
		name:      name,
		client:    c,
		listeners: []chan<- irc.Message{recvChan},
		mutex:     &sync.Mutex{},
	}

	h.clients[name] = client
	go h.clientWorker(client)

	return recvChan, sendChan, nil
}

func (h Handler) clientWorker(
	c *Client,
) {
LOOP:
	for {
		select {
		case m, ok := <-c.client.GetReceiveChannel():
			if !ok {
				break LOOP
			}
			c.mutex.Lock()
			newListeners := make([]chan<- irc.Message, 0, len(c.listeners))
			for _, l := range c.listeners {
				select {
				case l <- m:
					newListeners = append(newListeners, l)
				default:
					if h.verbose {
						log.Printf("IRC client listener is too slow: %s", c.name)
					}
					close(l)
				}
			}
			c.listeners = newListeners
			c.mutex.Unlock()
		case err, ok := <-c.client.GetErrorChannel():
			if !ok {
				break LOOP
			}
			if h.verbose {
				log.Printf("IRC client error: %s: %s", c.name, err)
			}
			break LOOP
		}
	}

	if h.verbose {
		log.Printf("Cleaning up IRC client: %s", c.name)
	}

	h.clientsMutex.Lock()
	defer h.clientsMutex.Unlock()

	c.mutex.Lock()
	defer c.mutex.Unlock()

	for _, l := range c.listeners {
		// TODO(horgh): Tell the websocket client something went wrong.
		close(l)
	}
	c.listeners = nil

	c.client.Stop()

	delete(h.clients, strings.ToLower(c.name))
}

func (w *WebClient) webSocketReader(
	ch chan<- map[string]string,
) {
	for {
		m, err := readWebSocket(w.conn)
		if err != nil {
			close(ch)
			w.logError(err)
			return
		}
		ch <- m
	}
}

func readWebSocket(conn *websocket.Conn) (
	map[string]string,
	error,
) {
	if err := conn.SetReadDeadline(time.Now().Add(
		10 * time.Minute)); err != nil {
		return nil, fmt.Errorf("error setting read deadline on websocket: %s",
			err)
	}

	var m map[string]string
	if err := conn.ReadJSON(&m); err != nil {
		return nil, fmt.Errorf("error reading JSON from websocket: %s", err)
	}

	return m, nil
}

func writeWebSocket(conn *websocket.Conn, v interface{}) error {
	if err := conn.SetWriteDeadline(time.Now().Add(
		30 * time.Second)); err != nil {
		return fmt.Errorf("error setting write deadline on websocket: %s",
			err)
	}

	if err := conn.WriteJSON(v); err != nil {
		return fmt.Errorf("error writing JSON from websocket: %s", err)
	}

	return nil
}

var joinRE = regexp.MustCompile(`(?i)/join\s+(#\S*)`)
var messageRE = regexp.MustCompile(`(?i)/msg\s+(\S+)\s+(.+)`)

// Do something with a message from a web client.
func (w *WebClient) handleClientMessage(
	sendChan chan<- irc.Message,
	m map[string]string,
) {
	if w.verbose {
		log.Printf("%s: read from websocket: %#v", w.remoteAddr, m)
	}
	message, ok := m["message"]
	if !ok || message == "" {
		w.logError(fmt.Errorf("no message provided"))
		return
	}

	if matches := joinRE.FindStringSubmatch(message); matches != nil {
		sendChan <- irc.Message{
			Command: "JOIN",
			Params:  []string{matches[1]},
		}
		return
	}

	if matches := messageRE.FindStringSubmatch(message); matches != nil {
		sendChan <- irc.Message{
			Command: "PRIVMSG",
			Params:  []string{matches[1], matches[2]},
		}
		return
	}

	w.logError(fmt.Errorf("unrecognized command: %#v", m))
}

// Do something with a message from an IRC client.
func (w *WebClient) handleIRCMessage(
	m irc.Message,
) {
	if w.verbose {
		log.Printf("%s: read from IRC: %s", w.remoteAddr, m)
	}

	if err := writeWebSocket(w.conn, m); err != nil {
		w.logError(err)
		return
	}
}
