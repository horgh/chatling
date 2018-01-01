package main

import (
	"flag"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sync"

	"github.com/gorilla/websocket"
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
