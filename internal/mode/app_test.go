package mode

import (
	"bytes"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/saxill/beamdrop/internal/engine"
	"github.com/saxill/beamdrop/internal/frame"
)

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func newTestApp(t *testing.T) *App {
	t.Helper()
	home := t.TempDir()
	a := NewApp(PortalOptions{
		InboxDir:      filepath.Join(home, "inbox"),
		KnownPeersDir: filepath.Join(home, "known"),
		ConfigDir:     filepath.Join(home, "config"),
		Port:          freePort(t),
	})
	t.Cleanup(a.Stop)
	return a
}

func (a *App) addr() string { return net.JoinHostPort("127.0.0.1", strconv.Itoa(a.opts.Port)) }

// autoAccept answers pairing prompts in the background, standing in for a
// user clicking the button. Every test that connects a peer needs this:
// App's confirmer blocks until the window replies, which is the point.
func autoAccept(t *testing.T, a *App) {
	t.Helper()
	done := make(chan struct{})
	t.Cleanup(func() { close(done) })
	go func() {
		for {
			select {
			case req := <-a.PairRequests():
				req.Accept()
			case <-done:
				return
			}
		}
	}()
}

// connectPeer dials the app as a peer would, returning its engine.
func connectPeer(t *testing.T, a *App, name, recvDir string) *engine.Engine {
	t.Helper()
	conn, err := net.DialTimeout("tcp", a.addr(), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	e, err := engine.New(engine.Config{
		Name: name, Conn: conn, IsInitiator: true,
		Confirmer: func(x, y [32]byte, n, c string, k bool) bool { return true },
	})
	if err != nil {
		t.Fatalf("pair as %s: %v", name, err)
	}
	if recvDir != "" {
		e.OnFileOffer(func(frame.FileOfferPayload) (string, bool) { return recvDir, true })
		go e.Serve()
	}
	return e
}

func TestAppStartStopIsIdempotent(t *testing.T) {
	a := newTestApp(t)
	if a.Status().Running {
		t.Error("reports running before Start")
	}
	if err := a.Start(); err != nil {
		t.Fatal(err)
	}
	if !a.Status().Running {
		t.Error("does not report running after Start")
	}
	// A window may call Start again on a button press; that must not try to
	// stand up a second server on the same port.
	if err := a.Start(); err != nil {
		t.Errorf("second Start: %v", err)
	}
	a.Stop()
	if a.Status().Running {
		t.Error("still reports running after Stop")
	}
	a.Stop() // must not panic
}

func TestAppStatusListsPeersAndURLs(t *testing.T) {
	a := newTestApp(t)
	if err := a.Start(); err != nil {
		t.Fatal(err)
	}
	autoAccept(t, a)

	st := a.Status()
	if len(st.URLs) == 0 {
		t.Error("Status has no URLs to show the user")
	}
	if st.InboxDir == "" {
		t.Error("Status has no inbox path")
	}

	connectPeer(t, a, "a-phone", "")

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if peers := a.Status().Peers; len(peers) == 1 && peers[0] == "a-phone" {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Errorf("Status().Peers = %v, want [a-phone]", a.Status().Peers)
}

// TestAppPairRequestBlocksUntilAnswered: a prompt the code sails past
// regardless is decoration, not a confirmation.
func TestAppPairRequestBlocksUntilAnswered(t *testing.T) {
	a := newTestApp(t)
	if err := a.Start(); err != nil {
		t.Fatal(err)
	}

	paired := make(chan error, 1)
	go func() {
		conn, err := net.DialTimeout("tcp", a.addr(), 5*time.Second)
		if err != nil {
			paired <- err
			return
		}
		defer conn.Close()
		_, err = engine.New(engine.Config{
			Name: "iPhone", Conn: conn, IsInitiator: true,
			Confirmer: func(x, y [32]byte, n, c string, k bool) bool { return true },
		})
		paired <- err
		time.Sleep(time.Second) // hold the connection briefly
	}()

	var req PairRequest
	select {
	case req = <-a.PairRequests():
	case <-time.After(5 * time.Second):
		t.Fatal("no pairing request surfaced to the UI")
	}
	if req.PeerName != "iPhone" {
		t.Errorf("PeerName = %q", req.PeerName)
	}
	if len(req.Code) != 6 {
		t.Errorf("Code = %q, want 6 digits", req.Code)
	}

	select {
	case err := <-paired:
		t.Fatalf("pairing completed before the UI answered (err=%v)", err)
	case <-time.After(300 * time.Millisecond):
	}

	req.Accept()
	select {
	case err := <-paired:
		if err != nil {
			t.Errorf("pairing failed after Accept: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("pairing did not complete after Accept")
	}
}

func TestAppRejectedPairingFails(t *testing.T) {
	a := newTestApp(t)
	if err := a.Start(); err != nil {
		t.Fatal(err)
	}
	paired := make(chan error, 1)
	go func() {
		conn, err := net.DialTimeout("tcp", a.addr(), 5*time.Second)
		if err != nil {
			paired <- err
			return
		}
		defer conn.Close()
		_, err = engine.New(engine.Config{
			Name: "stranger", Conn: conn, IsInitiator: true,
			Confirmer: func(x, y [32]byte, n, c string, k bool) bool { return true },
		})
		paired <- err
	}()

	select {
	case req := <-a.PairRequests():
		req.Reject()
	case <-time.After(5 * time.Second):
		t.Fatal("no pairing request surfaced")
	}
	select {
	case err := <-paired:
		if err == nil {
			t.Error("pairing succeeded despite being rejected in the UI")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("rejected pairing never returned")
	}
}

func TestAppInboxIsNewestFirst(t *testing.T) {
	a := newTestApp(t)
	if err := os.MkdirAll(a.opts.InboxDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for i, name := range []string{"old.txt", "middle.txt", "new.txt"} {
		p := filepath.Join(a.opts.InboxDir, name)
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		when := time.Now().Add(time.Duration(i-3) * time.Hour)
		if err := os.Chtimes(p, when, when); err != nil {
			t.Fatal(err)
		}
	}
	got := a.Inbox(0)
	if len(got) != 3 {
		t.Fatalf("Inbox() = %d files, want 3", len(got))
	}
	if got[0].Name != "new.txt" || got[2].Name != "old.txt" {
		t.Errorf("order = %s, %s, %s; want newest first", got[0].Name, got[1].Name, got[2].Name)
	}
	if n := len(a.Inbox(2)); n != 2 {
		t.Errorf("Inbox(2) returned %d, want 2", n)
	}
}

// TestAppSendWithNobodyConnectedSaysSo: dropping a file on a window with no
// peers is a mistake worth naming, not a silent no-op.
func TestAppSendWithNobodyConnectedSaysSo(t *testing.T) {
	a := newTestApp(t)
	src := filepath.Join(t.TempDir(), "f.bin")
	if err := os.WriteFile(src, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := a.Send(src); err == nil {
		t.Error("Send before Start reported success")
	}
	if err := a.Start(); err != nil {
		t.Fatal(err)
	}
	n, err := a.Send(src)
	if err == nil {
		t.Error("Send with no peers reported success")
	}
	if n != 0 {
		t.Errorf("Send reported %d recipients with none connected", n)
	}
}

func TestAppSendReachesConnectedPeers(t *testing.T) {
	a := newTestApp(t)
	if err := a.Start(); err != nil {
		t.Fatal(err)
	}
	autoAccept(t, a)

	recvA, recvB := t.TempDir(), t.TempDir()
	connectPeer(t, a, "phone-a", recvA)
	connectPeer(t, a, "phone-b", recvB)

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && len(a.Status().Peers) < 2 {
		time.Sleep(50 * time.Millisecond)
	}

	body := bytes.Repeat([]byte("desktop-drop"), 3000)
	src := filepath.Join(t.TempDir(), "dropped.bin")
	if err := os.WriteFile(src, body, 0o644); err != nil {
		t.Fatal(err)
	}

	n, err := a.Send(src)
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if n != 2 {
		t.Errorf("Send reached %d peers, want 2", n)
	}
	for _, dir := range []string{recvA, recvB} {
		got, err := os.ReadFile(filepath.Join(dir, "dropped.bin"))
		if err != nil {
			t.Errorf("file did not arrive in %s: %v", dir, err)
			continue
		}
		if !bytes.Equal(got, body) {
			t.Errorf("file in %s differs", dir)
		}
	}
}

func TestAppOnChangeFiresOnActivity(t *testing.T) {
	a := newTestApp(t)
	fired := make(chan struct{}, 16)
	a.OnChange(func() {
		select {
		case fired <- struct{}{}:
		default:
		}
	})
	if err := a.Start(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-fired:
	case <-time.After(3 * time.Second):
		t.Error("OnChange never fired, so a window would never redraw")
	}
}
