package main

import (
	"flag"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	ircclient "github.com/horgh/boxcat"
	"github.com/horgh/irc"
)

func main() {
	args, err := getArgs()
	if err != nil {
		printUsage(err)
		os.Exit(1)
	}

	h, err := newHandler(args)
	if err != nil {
		log.Fatalf("error creating handler: %s", err)
	}

	log.Printf("chatling ready")
	if err := http.ListenAndServe(args.address, h); err != nil {
		log.Fatalf("error serving: %s", err)
	}
}

// Args are command line arguments.
type Args struct {
	address   string
	dir       string
	verbose   bool
	ircServer string
	ircPort   uint16
}

func getArgs() (*Args, error) {
	address := flag.String("address", "localhost:8080", "Listen address")
	dir := flag.String("dir", "static",
		"Path to directory containing static files (templates, assets)")
	verbose := flag.Bool("verbose", false, "Enable verbose logs")
	ircServer := flag.String("irc-server", "localhost", "IRC server hostname")
	ircPort := flag.Int("irc-port", 6667, "IRC server port")

	flag.Parse()

	if *address == "" {
		return nil, fmt.Errorf("you must specify a listen address")
	}

	if *dir == "" {
		return nil, fmt.Errorf("you must specify the asset directory")
	}

	if *ircServer == "" {
		return nil, fmt.Errorf("you must specify an IRC server")
	}

	return &Args{
		address:   *address,
		dir:       *dir,
		verbose:   *verbose,
		ircServer: *ircServer,
		ircPort:   uint16(*ircPort),
	}, nil
}

func printUsage(err error) {
	fmt.Fprintf(os.Stderr, "%s\n", err)
	fmt.Fprintf(os.Stderr, "Usage: %s: <arguments>\n", os.Args[0])
	flag.PrintDefaults()
}

// Handler responds to HTTP requests.
type Handler struct {
	dir             string
	upgrader        *websocket.Upgrader
	verbose         bool
	ircServer       string
	ircPort         uint16
	ircClients      map[string]*IRCClient
	ircClientsMutex *sync.Mutex
}

func newHandler(args *Args) (Handler, error) {
	// We could parse the templates here, but it's more convenient to do so on
	// demand so we don't have to restart this program to see new HTML/JS.

	return Handler{
		dir:             args.dir,
		upgrader:        &websocket.Upgrader{},
		verbose:         args.verbose,
		ircServer:       args.ircServer,
		ircPort:         args.ircPort,
		ircClients:      map[string]*IRCClient{},
		ircClientsMutex: &sync.Mutex{},
	}, nil
}

// ServeHTTP responds to an HTTP request.
func (h Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == "GET" && r.URL.Path == "/" {
		h.indexRequest(w, r)
		return
	}

	if r.Method == "GET" && r.URL.Path == "/chat" {
		h.chatRequest(w, r)
		return
	}

	h.errorRequest(w, r, http.StatusBadRequest, fmt.Errorf("path not found"))
}

func (h Handler) indexRequest(w http.ResponseWriter, r *http.Request) {
	t, err := template.ParseFiles(filepath.Join(h.dir, "index.html"))
	if err != nil {
		h.logError(r, fmt.Errorf("error parsing index template: %s", err))
		return
	}

	if err := t.Execute(w, nil); err != nil {
		h.logError(r, fmt.Errorf("error executing index template: %s", err))
		return
	}
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
	go h.webSocketReader(r, conn, webRecvChan)

LOOP:
	for {
		select {
		case m, ok := <-webRecvChan:
			if !ok {
				break LOOP
			}
			h.handleClientMessage(r, sendChan, m)
		case m, ok := <-recvChan:
			if !ok {
				break LOOP
			}
			h.handleIRCMessage(r, conn, m)
		}
	}

	// We could remove the listener from the IRCClient's listeners. It will be
	// cleaned up when it blocks though.
}

// IRCClient holds an IRC client connection and provides access to it from
// multiple goroutines.
//
// An *ircclient.Client is really only usable by a single goroutine. Primarily
// because it publishes each message only once. In order to handle multiple web
// clients all using the same one, we wrap around it here and deliver the
// message to each.
type IRCClient struct {
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

	h.ircClientsMutex.Lock()
	defer h.ircClientsMutex.Unlock()

	client, ok := h.ircClients[name]
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

	client = &IRCClient{
		name:      name,
		client:    c,
		listeners: []chan<- irc.Message{recvChan},
		mutex:     &sync.Mutex{},
	}

	h.ircClients[name] = client
	go h.clientWorker(client)

	return recvChan, sendChan, nil
}

func (h Handler) clientWorker(
	c *IRCClient,
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

	h.ircClientsMutex.Lock()
	defer h.ircClientsMutex.Unlock()

	c.mutex.Lock()
	defer c.mutex.Unlock()

	for _, l := range c.listeners {
		// TODO(horgh): Tell the websocket client something went wrong.
		close(l)
	}
	c.listeners = nil

	c.client.Stop()

	delete(h.ircClients, strings.ToLower(c.name))
}

func (h Handler) webSocketReader(
	r *http.Request,
	conn *websocket.Conn,
	ch chan<- map[string]string,
) {
	for {
		m, err := readWebSocket(conn)
		if err != nil {
			close(ch)
			h.logError(r, err)
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
func (h Handler) handleClientMessage(
	r *http.Request,
	sendChan chan<- irc.Message,
	m map[string]string,
) {
	if h.verbose {
		log.Printf("%s: read from websocket: %#v", r.RemoteAddr, m)
	}
	message, ok := m["message"]
	if !ok || message == "" {
		h.logError(r, fmt.Errorf("no message provided"))
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

	h.logError(r, fmt.Errorf("unrecognized command: %#v", m))
}

// Do something with a message from an IRC client.
func (h Handler) handleIRCMessage(
	r *http.Request,
	conn *websocket.Conn,
	m irc.Message,
) {
	if h.verbose {
		log.Printf("%s: read from IRC: %s", r.RemoteAddr, m)
	}

	if err := writeWebSocket(conn, m); err != nil {
		h.logError(r, err)
		return
	}
}

func (h Handler) errorRequest(
	w http.ResponseWriter,
	r *http.Request,
	status int,
	origErr error,
) {
	h.logError(r, origErr)

	w.WriteHeader(http.StatusBadRequest)

	t, err := template.ParseFiles(filepath.Join(h.dir, "error.html"))
	if err != nil {
		h.logError(r, fmt.Errorf("error parsing error template: %s", err))
		return
	}

	if err := t.Execute(w, origErr.Error()); err != nil {
		h.logError(r, fmt.Errorf("error executing error template: %s", err))
		return
	}
}

func (h Handler) logError(r *http.Request, err error) {
	log.Printf("%s: %s: %s", r.RemoteAddr, r.RequestURI, err)
}
