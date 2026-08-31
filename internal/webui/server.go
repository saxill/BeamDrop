package webui

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"

	"github.com/gorilla/websocket"
)

type ServeOptions struct {
	Port int
	// StaticDir serves the web UI off disk instead of from the copy
	// embedded in the binary. Leave it empty in production; it exists for
	// tests and for editing the page without rebuilding.
	StaticDir string
	// CertDir persists the self-signed TLS keypair so the phone does not
	// have to re-accept a brand-new certificate on every restart. Empty
	// means generate an ephemeral one.
	CertDir string
	// Listener, when set, is served instead of opening Port. The portal
	// passes a netmux.Listener so raw beamdrop peers and the phone's HTTPS
	// session can share one port; Port is ignored in that case.
	Listener  net.Listener
	OnConnect func(ctx context.Context, conn net.Conn) error // engine hook: called when a new virtual peer connects
	// Routes are mounted alongside the page, before the catch-all. The
	// portal mounts /upload here so the https URL it prints for the phone
	// also accepts a file, rather than 404ing on the one address people are
	// told to use. Each handler does its own authentication.
	Routes map[string]http.Handler
}

func Serve(ctx context.Context, opts ServeOptions) error {
	if opts.Port == 0 {
		opts.Port = 4747
	}
	tlsCfg, err := serverTLS(opts.CertDir)
	if err != nil {
		return err
	}
	mux := http.NewServeMux()
	for pattern, h := range opts.Routes {
		mux.Handle(pattern, h)
	}
	page := staticHandler(opts.StaticDir)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// Nothing here answers a POST, so one arriving is a client aiming at
		// a route that does not exist — an upload with the path spelled
		// wrong, most often. Say so instead of returning a bare 404 that
		// looks, from the laptop, like nothing was ever sent.
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			log.Printf("webui: %s %s from %s matched no route", r.Method, r.URL.Path, r.RemoteAddr)
		}
		page.ServeHTTP(w, r)
	})
	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		c, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		ws := newWSShim(c)
		if opts.OnConnect != nil {
			if err := opts.OnConnect(ctx, ws); err != nil {
				log.Printf("webui: connection handler: %v", err)
			}
		}
	})
	srv := &http.Server{
		Addr:      fmt.Sprintf(":%d", opts.Port),
		Handler:   mux,
		TLSConfig: tlsCfg,
	}
	go func() {
		<-ctx.Done()
		srv.Shutdown(context.Background())
	}()
	// Certs come from TLSConfig in both branches, hence the empty paths.
	if opts.Listener != nil {
		return srv.ServeTLS(opts.Listener, "", "")
	}
	return srv.ListenAndServeTLS("", "")
}
