package mode

import (
	"bytes"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/saxill/beamdrop/internal/engine"
	"github.com/saxill/beamdrop/internal/frame"
)

func TestWatchSendsFileOnCreate(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	addr := ln.Addr().String()

	recvDir := t.TempDir()
	accept := func(o frame.FileOfferPayload) (string, bool) {
		return recvDir, true
	}
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		e, err := engine.New(engine.Config{
			Name: "B", Conn: conn, IsInitiator: false,
			Confirmer: func(a, b [32]byte, name, code string, known bool) bool { return true },
		})
		if err != nil {
			t.Errorf("responder: %v", err)
			return
		}
		e.OnFileOffer(accept)
		_ = e.Serve()
	}()

	watchDir := t.TempDir()
	connect := func() (*engine.Engine, error) {
		conn, err := net.Dial("tcp", addr)
		if err != nil {
			return nil, err
		}
		return engine.New(engine.Config{
			Name: "A", Conn: conn, IsInitiator: true,
			Confirmer: func(a, b [32]byte, name, code string, known bool) bool { return true },
		})
	}

	// Watch never returns on its own (no shutdown hook in this MVP-era
	// design — matches the CLI's Ctrl-C-driven lifecycle). Run it in a
	// goroutine and let it leak past the end of this test; it's blocked
	// on channel reads and does no harm.
	//
	// ready is signaled once the fsnotify watcher is armed, so the write
	// below cannot race the watcher's startup. A blind retry-write here
	// (an earlier version of this test did that) is unsafe: an in-flight
	// Sender may already have the file open and mid-read, and repeatedly
	// truncating/replacing the same path out from under that read
	// corrupts the transfer instead of just missing an event.
	ready := make(chan struct{})
	go Watch(watchDir, WatchOptions{Connect: connect, ready: ready})
	<-ready

	want := bytes.Repeat([]byte("hi"), 1000)
	notePath := filepath.Join(watchDir, "note.txt")
	recvPath := filepath.Join(recvDir, "note.txt")

	if err := os.WriteFile(notePath, want, 0o644); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		got, err := os.ReadFile(recvPath)
		if err == nil {
			if !bytes.Equal(got, want) {
				t.Fatalf("received %d bytes, want %d bytes matching", len(got), len(want))
			}
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("timeout waiting for watched file to arrive")
}
