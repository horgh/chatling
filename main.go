// TODO(horgh): If there's no webclient connected for the IRC client we still
// need to process messages from the IRC client. Otherwise we'll end up
// blocking on the channels there and ceasing to respond to PINGs.
//
// TODO(horgh): Also we need to be able to reap dead clients.
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
	dir        string
	upgrader   *websocket.Upgrader
	verbose    bool
	ircServer  string
	ircPort    uint16
	ircClients map[string]*ircclient.Client
}

func newHandler(args *Args) (Handler, error) {
	// We could parse the templates here, but it's more convenient to do so on
	// demand so we don't have to restart this program to see new HTML/JS.

	return Handler{
		dir:        args.dir,
		upgrader:   &websocket.Upgrader{},
		verbose:    args.verbose,
		ircServer:  args.ircServer,
		ircPort:    args.ircPort,
		ircClients: map[string]*ircclient.Client{},
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

	// The first message should provide info we need to set up the connection.
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

	client, err := h.getIRCClient(name)
	if err != nil {
		h.logError(r, fmt.Errorf("error starting IRC client: %s", err))
		return
	}

	webRecvChan := make(chan map[string]string, 64)
	go h.webSocketReader(r, conn, webRecvChan)

	for {
		select {
		case m, ok := <-webRecvChan:
			if !ok {
				return
			}
			h.handleClientMessage(r, client, m)
		case m, ok := <-client.GetReceiveChannel():
			if !ok {
				return
			}
			h.handleIRCMessage(r, conn, name, client, m)
		case err, ok := <-client.GetErrorChannel():
			if !ok {
				return
			}
			h.logError(r, fmt.Errorf("error from IRC: %s", err))
			delete(h.ircClients, strings.ToLower(name))
			client.Stop()
			// TODO(horgh): Tell the websocket client something went wrong.
			return
		}
	}
}

func (h Handler) getIRCClient(name string) (*ircclient.Client, error) {
	{
		client, ok := h.ircClients[strings.ToLower(name)]
		if ok {
			return client, nil
		}
	}

	client := ircclient.NewClient(name, h.ircServer, h.ircPort)
	if _, _, _, err := client.Start(); err != nil {
		return nil, fmt.Errorf("error starting IRC client: %s", err)
	}

	h.ircClients[strings.ToLower(name)] = client

	return client, nil
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
	client *ircclient.Client,
	m map[string]string,
) {
	if h.verbose {
		log.Printf("%s: read from websocket: %s", r.RemoteAddr, m)
	}

	message, ok := m["message"]
	if !ok || message == "" {
		h.logError(r, fmt.Errorf("no message provided"))
		return
	}

	if matches := joinRE.FindStringSubmatch(message); matches != nil {
		client.GetSendChannel() <- irc.Message{
			Command: "JOIN",
			Params:  []string{matches[1]},
		}
		return
	}

	if matches := messageRE.FindStringSubmatch(message); matches != nil {
		client.GetSendChannel() <- irc.Message{
			Command: "PRIVMSG",
			Params:  []string{matches[1], matches[2]},
		}
		return
	}
}

// Do something with a message from an IRC client.
func (h Handler) handleIRCMessage(
	r *http.Request,
	conn *websocket.Conn,
	name string,
	client *ircclient.Client,
	m irc.Message,
) {
	if h.verbose {
		log.Printf("%s: read from IRC: %s", r.RemoteAddr, m)
	}

	if err := writeWebSocket(conn, m); err != nil {
		h.logError(r, err)
		return
	}

	if m.Command == "ERROR" {
		h.logError(r, fmt.Errorf("ERROR from IRC client: %s", m))
		delete(h.ircClients, strings.ToLower(name))
		client.Stop()
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
