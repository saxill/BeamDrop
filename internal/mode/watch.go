package mode

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/saxill/beamdrop/internal/config"
	"github.com/saxill/beamdrop/internal/engine"
	"github.com/saxill/beamdrop/internal/transfer"
)

type SenderFactory func() (*engine.Engine, error)

type WatchOptions struct {
	Verbose bool
	// Peer and Port name the portal to ship to. Empty/zero means loopback
	// on the default port.
	Peer string
	Port int
	// Connect dials and pairs with the peer once, at watch startup. The
	// returned engine is reused for every subsequent file event —
	// Engine.SendFile does not close or consume the connection, so a hot
	// folder streaming many files does not re-pair per file. If nil,
	// Watch dials Peer:Port as the initiator.
	Connect SenderFactory

	// ready is signaled (by closing) once the fsnotify watcher is armed,
	// right after w.Add succeeds. Test-only seam: unexported, so it is
	// unreachable from outside this package. Lets a test block until the
	// watcher genuinely cannot miss a subsequent write, instead of
	// guessing with a sleep or retry-writing the same path (which can
	// race an in-flight Sender's open file handle).
	ready chan<- struct{}
}

func Watch(dir string, opts WatchOptions) error {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	if info, err := os.Stat(abs); err != nil || !info.IsDir() {
		return fmt.Errorf("watch: %s is not a directory", abs)
	}

	w, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	defer w.Close()
	if err := w.Add(abs); err != nil {
		return err
	}
	if opts.ready != nil {
		close(opts.ready)
	}

	connect := opts.Connect
	if connect == nil {
		connect = defaultConnect(opts)
	}
	e, err := connect()
	if err != nil {
		return fmt.Errorf("watch: connect: %w", err)
	}

	fmt.Printf("beamdrop: watching %s (Ctrl-C to stop)\n", abs)
	for {
		select {
		case ev, ok := <-w.Events:
			if !ok {
				return nil
			}
			if ev.Op&(fsnotify.Create|fsnotify.Write) != 0 {
				if opts.Verbose {
					fmt.Printf("event: %s %s\n", ev.Op, ev.Name)
				}
				sendWatched(e, ev.Name)
			}
		case err, ok := <-w.Errors:
			if !ok {
				return nil
			}
			fmt.Fprintf(os.Stderr, "watch error: %v\n", err)
		}
	}
}

func sendWatched(e *engine.Engine, path string) {
	s, err := transfer.NewSender(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "watch: %s: %v\n", path, err)
		return
	}
	defer s.Close()
	if err := e.SendFile(s, nil); err != nil {
		fmt.Fprintf(os.Stderr, "watch: send %s: %v\n", path, err)
		return
	}
	fmt.Printf("beamdrop: sent %s\n", path)
}

func defaultConnect(opts WatchOptions) SenderFactory {
	return func() (*engine.Engine, error) {
		cfg := config.Defaults()
		port := opts.Port
		if port == 0 {
			port = cfg.Port
		}
		identity, known, err := localIdentity(cfg)
		if err != nil {
			return nil, err
		}
		addr, how, err := findPortal(opts.Peer, port, known)
		if err != nil {
			return nil, fmt.Errorf("watch: %w", err)
		}
		if how != "" {
			fmt.Printf("beamdrop: connecting to %s (%s)\n", addr, how)
		}
		conn, err := net.DialTimeout("tcp", addr, 10*time.Second)
		if err != nil {
			return nil, fmt.Errorf("watch: dial %s: %w (is `beamdrop portal` running there?)", addr, err)
		}
		return engine.New(engine.Config{
			Name:        hostName(),
			Conn:        conn,
			IsInitiator: true,
			Identity:    &identity,
			Known:       known,
			Confirmer: func(initPub, respPub [32]byte, peerName, code string, isKnown bool) bool {
				if !isKnown {
					fmt.Printf("beamdrop: pairing with %s — code %s\n", peerName, code)
					fmt.Println("beamdrop: confirm the same code in the portal to continue")
				}
				return true
			},
		})
	}
}
