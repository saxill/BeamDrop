// Package smoke drives a real webui.Serve + engine.Engine against the
// actual production pairing/protocol JS (running under Node as a child
// process), over a live TLS WebSocket. This is the only test in the repo
// that exercises the full Go<->JS boundary end to end rather than testing
// either side in isolation.
package smoke

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/saxill/beamdrop/internal/engine"
	"github.com/saxill/beamdrop/internal/transfer"
	"github.com/saxill/beamdrop/internal/webui"
)

func requireNode(t *testing.T) string {
	path, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH, skipping JS/Go smoke test")
	}
	return path
}

// TestSmokeLaptopSendsToFakeIPhone starts a real webui.Serve, connects a
// Node-driven "fake iPhone" (running the actual production pairing.js /
// protocol.js) as the receiver, and pushes a file from the Go side —
// exercising the full pairing ceremony and file transfer across the
// Go<->JS language boundary.
func TestSmokeLaptopSendsToFakeIPhone(t *testing.T) {
	node := requireNode(t)
	repoRoot := findRepoRoot(t)

	srcDir := t.TempDir()
	recvDir := t.TempDir()
	srcPath := filepath.Join(srcDir, "smoke.bin")
	want := strings.Repeat("beamdrop-smoke-", 6000) // ~88KB (15 * 6000), crosses the 64KB chunk boundary
	if err := os.WriteFile(srcPath, []byte(want), 0o644); err != nil {
		t.Fatal(err)
	}

	const port = 47470 // fixed high port for the smoke test; adjust if occupied
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	serveErr := make(chan error, 1)
	connected := make(chan *engine.Engine, 1)
	go func() {
		serveErr <- webui.Serve(ctx, webui.ServeOptions{
			Port:      port,
			StaticDir: filepath.Join(repoRoot, "internal/webui/static"),
			OnConnect: func(ctx context.Context, conn net.Conn) error {
				e, err := engine.New(engine.Config{
					Name: "smoke-laptop", Conn: conn, IsInitiator: false,
					Confirmer: func(a, b [32]byte, name, code string, known bool) bool { return true },
				})
				if err != nil {
					return err
				}
				connected <- e
				// Filter the same way the portal does, so a fake iPhone
				// exiting normally does not print as a server error.
				if err := e.Serve(); !engine.IsDisconnect(err) {
					return err
				}
				return nil
			},
		})
	}()

	cmd := exec.Command(node, "--no-warnings", filepath.Join(repoRoot, "internal/smoke/fake_iphone.mjs"),
		"wss://127.0.0.1:47470/ws", "receive", recvDir)
	cmd.Dir = repoRoot
	// The webui server uses a self-signed cert; disable Node's TLS
	// verification for this child process only (not the Go side, which
	// doesn't verify anything here either — it's the server). --no-warnings
	// silences the resulting "insecure" experimental-warning noise that
	// NODE_TLS_REJECT_UNAUTHORIZED=0 otherwise prints to stderr on every run.
	cmd.Env = append(os.Environ(), "NODE_TLS_REJECT_UNAUTHORIZED=0")
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Wait()

	var e *engine.Engine
	select {
	case e = <-connected:
	case <-time.After(10 * time.Second):
		t.Fatal("laptop-side engine never connected")
	}

	s, err := transfer.NewSender(srcPath)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := e.SendFile(s, nil); err != nil {
		t.Fatalf("SendFile: %v", err)
	}

	waitForJSONStep(t, out, "received", 15*time.Second)

	got, err := os.ReadFile(filepath.Join(recvDir, "smoke.bin"))
	if err != nil {
		t.Fatalf("read received file: %v", err)
	}
	if string(got) != want {
		t.Errorf("received %d bytes, want %d bytes matching", len(got), len(want))
	}
}

func waitForJSONStep(t *testing.T, r io.Reader, step string, timeout time.Duration) {
	t.Helper()
	type line struct {
		Step string `json:"step"`
	}
	dec := json.NewDecoder(r)
	done := make(chan struct{})
	go func() {
		for {
			var l line
			if err := dec.Decode(&l); err != nil {
				return
			}
			if l.Step == step {
				close(done)
				return
			}
			if l.Step == "error" {
				t.Errorf("fake_iphone.mjs reported an error step")
				close(done)
				return
			}
		}
	}()
	select {
	case <-done:
	case <-time.After(timeout):
		t.Fatal("timeout waiting for fake_iphone step: " + step)
	}
}

func findRepoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	dir := wd
	for i := 0; i < 6; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		dir = filepath.Dir(dir)
	}
	t.Fatal("go.mod not found walking up from " + wd)
	return ""
}
