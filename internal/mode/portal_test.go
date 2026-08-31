package mode

import (
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/saxill/beamdrop/internal/engine"
	"github.com/saxill/beamdrop/internal/frame"
)

func TestModelPairRequestPromptsThenRespondsYes(t *testing.T) {
	pairCh := make(chan pairRequest)
	resultCh := make(chan sendResult)
	logCh := make(chan string)
	m := initialModel("/inbox", engine.NewRegistry(), pairCh, resultCh, logCh)

	req := pairRequest{name: "iPhone", code: "123456", respond: make(chan bool, 1)}
	updated, _ := m.Update(req)
	m = updated.(model)
	if m.pending == nil {
		t.Fatal("pending is nil after pairRequest")
	}
	found := false
	for _, line := range m.log {
		if line == "pairing: iPhone wants to connect (code 123456) — [y/n]" {
			found = true
		}
	}
	if !found {
		t.Errorf("log = %v, missing pairing prompt line", m.log)
	}

	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	m = updated.(model)
	if m.pending != nil {
		t.Error("pending should be nil after y")
	}
	select {
	case got := <-req.respond:
		if !got {
			t.Error("respond channel got false, want true")
		}
	case <-time.After(time.Second):
		t.Fatal("respond channel never received a value")
	}
}

func TestModelSendFansOutToAllRegisteredPeers(t *testing.T) {
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "src.bin")
	// write a small file
	writeTestFile(t, srcPath, 5000)

	registry := engine.NewRegistry()
	recvDirs := make([]string, 2)
	for i := range recvDirs {
		recvDirs[i] = filepath.Join(dir, "recv", string(rune('A'+i)))
		e, cleanup := pairedTestEngine(t, recvDirs[i])
		t.Cleanup(cleanup)
		registry.Add(e)
	}

	resultCh := make(chan sendResult, len(recvDirs))
	logCh := make(chan string)
	m := initialModel(dir, registry, make(chan pairRequest), resultCh, logCh)
	m.input = ":send " + srcPath
	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(model)
	_ = cmd

	for i := 0; i < len(recvDirs); i++ {
		select {
		case res := <-resultCh:
			if res.err != nil {
				t.Errorf("send result: %v", res.err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("timeout waiting for send results")
		}
	}
	for _, dir := range recvDirs {
		assertFileArrived(t, filepath.Join(dir, "src.bin"), 5000)
	}
}

// writeTestFile writes n bytes of deterministic content to path, creating any
// parent directories as needed.
func writeTestFile(t *testing.T, path string, n int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	data := make([]byte, n)
	for i := range data {
		data[i] = byte(i % 256)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// pairedTestEngine builds a TCP loopback pair (like engine_test.go's newPipe
// helper, but engine.newPipe is unexported and lives in package engine, so
// this is its own equivalent) and runs two engine.New calls, one per side.
// It returns the initiator side (ready to register into a Registry and use
// for SendFile) and a cleanup func that closes both connections.
func pairedTestEngine(t *testing.T, recvDir string) (*engine.Engine, func()) {
	t.Helper()
	if err := os.MkdirAll(recvDir, 0o755); err != nil {
		t.Fatal(err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()

	type initResult struct {
		e    *engine.Engine
		conn net.Conn
		err  error
	}
	initCh := make(chan initResult, 1)
	go func() {
		conn, err := net.Dial("tcp", addr)
		if err != nil {
			initCh <- initResult{err: err}
			return
		}
		e, err := engine.New(engine.Config{
			Name:        "initiator",
			Conn:        conn,
			IsInitiator: true,
			Confirmer:   func(a, b [32]byte, name, code string, known bool) bool { return true },
		})
		initCh <- initResult{e: e, conn: conn, err: err}
	}()

	respConn, err := ln.Accept()
	if err != nil {
		ln.Close()
		t.Fatal(err)
	}
	ln.Close()

	respE, err := engine.New(engine.Config{
		Name:        "responder",
		Conn:        respConn,
		IsInitiator: false,
		Confirmer:   func(a, b [32]byte, name, code string, known bool) bool { return true },
	})
	if err != nil {
		t.Fatal(err)
	}
	respE.OnFileOffer(func(frame.FileOfferPayload) (string, bool) {
		return recvDir, true
	})
	go respE.Serve()

	res := <-initCh
	if res.err != nil {
		t.Fatal(res.err)
	}

	cleanup := func() {
		res.conn.Close()
		respConn.Close()
	}
	return res.e, cleanup
}

// assertFileArrived polls path with a timeout until it exists and has the
// expected length, matching the pattern from Task 18's
// TestWatchSendsFileOnCreate.
func assertFileArrived(t *testing.T, path string, wantLen int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		info, err := os.Stat(path)
		if err == nil && info.Size() == int64(wantLen) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s to arrive with %d bytes", path, wantLen)
}
