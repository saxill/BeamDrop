package mode

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/saxill/beamdrop/internal/pairing"
	"github.com/saxill/beamdrop/internal/peer"
	"github.com/saxill/beamdrop/internal/spool"
)

// relayFixture stands up a relay node, plus a "laptop" that can be started
// and stopped so the offline case is real rather than mocked.
type relayFixture struct {
	relay      *peerServer
	relaySpool *spool.Spool
	token      string
	laptopName string
	laptopKP   pairing.KeyPair
	laptopDir  string // the laptop's inbox
	laptopPort int
}

func newRelayFixture(t *testing.T) *relayFixture {
	t.Helper()

	// The laptop's identity has to be stable across its restarts, and the
	// relay has to already know it — a relay runs unattended and refuses to
	// pair with a stranger.
	laptopKP, err := pairing.LoadOrCreateIdentity(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	laptopName := "laptop-under-test"

	store := peer.NewMemStore()
	store.Add(peer.Peer{Name: laptopName, PubKey: laptopKP.Pub, LastIP: "127.0.0.1"})
	relayKnown, err := pairing.NewKnownPeers(t.TempDir(), store)
	if err != nil {
		t.Fatal(err)
	}
	relayKP, err := pairing.LoadOrCreateIdentity(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	sp, err := spool.Open(filepath.Join(t.TempDir(), "spool"))
	if err != nil {
		t.Fatal(err)
	}

	// The laptop listens on a fixed port so the relay's remembered address
	// stays valid across the stop/start below.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	laptopPort := ln.Addr().(*net.TCPAddr).Port
	ln.Close()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	relay, err := startPeerServer(ctx, peerServerOptions{
		ListenAddr:      "127.0.0.1:0",
		InboxDir:        t.TempDir(),
		Known:           relayKnown,
		Identity:        &relayKP,
		Spool:           sp,
		PeerPort:        laptopPort,
		ForwardInterval: 300 * time.Millisecond,
		UploadToken:     testToken,
		Confirmer:       func(a, b [32]byte, n, c string, k bool) bool { return true },
		Logf:            func(string, ...any) {},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { relay.Close() })

	return &relayFixture{
		relay:      relay,
		relaySpool: sp,
		token:      testToken,
		laptopName: laptopName,
		laptopKP:   laptopKP,
		laptopDir:  t.TempDir(),
		laptopPort: laptopPort,
	}
}

// startLaptop brings the destination online. Returns a stop func.
func (f *relayFixture) startLaptop(t *testing.T) func() {
	t.Helper()
	known, err := pairing.NewKnownPeers(t.TempDir(), peer.NewMemStore())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	srv, err := startPeerServer(ctx, peerServerOptions{
		ListenAddr: fmt.Sprintf("127.0.0.1:%d", f.laptopPort),
		InboxDir:   f.laptopDir,
		Known:      known,
		Identity:   &f.laptopKP,
		Confirmer:  func(a, b [32]byte, n, c string, k bool) bool { return true },
		Logf:       func(string, ...any) {},
	})
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	return func() { cancel(); srv.Close(); time.Sleep(100 * time.Millisecond) }
}

func (f *relayFixture) upload(t *testing.T, name, to string, body []byte) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost,
		"http://"+f.relay.Addr().String()+"/upload", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+f.token)
	req.Header.Set("X-Beamdrop-Filename", name)
	if to != "" {
		req.Header.Set("X-Beamdrop-To", to)
	}
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// TestRelayHoldsFileWhileLaptopIsOffThenDeliversIt is the whole feature:
// the phone hands a file to a machine that is always up, that machine
// accepts it even though the laptop is shut, and the file lands on the
// laptop by itself once it comes back.
func TestRelayHoldsFileWhileLaptopIsOffThenDeliversIt(t *testing.T) {
	f := newRelayFixture(t)
	body := bytes.Repeat([]byte("holiday-photo"), 4000) // ~52KB, crosses a chunk

	// Laptop is off. The upload still succeeds — that is the point.
	resp := f.upload(t, "IMG_9999.jpeg", f.laptopName, body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("upload while destination offline = %d, want 200", resp.StatusCode)
	}

	pending, err := f.relaySpool.PendingFor(f.laptopName)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 {
		t.Fatalf("spool holds %d items for the laptop, want 1", len(pending))
	}
	if _, err := os.Stat(filepath.Join(f.laptopDir, "IMG_9999.jpeg")); err == nil {
		t.Fatal("file reached the laptop while it was supposed to be offline")
	}

	// Laptop comes back.
	stop := f.startLaptop(t)
	defer stop()

	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		got, err := os.ReadFile(filepath.Join(f.laptopDir, "IMG_9999.jpeg"))
		if err == nil && len(got) == len(body) {
			if !bytes.Equal(got, body) {
				t.Fatal("delivered file differs from what was uploaded")
			}
			// The relay holds the only copy, so it must not drop it until
			// the destination confirmed the hash.
			remaining, _ := f.relaySpool.PendingFor(f.laptopName)
			if len(remaining) != 0 {
				t.Errorf("spool still holds %d items after delivery", len(remaining))
			}
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	pending, _ = f.relaySpool.PendingFor(f.laptopName)
	t.Fatalf("file never arrived; %d still spooled (last error: %s)",
		len(pending), lastErrorOf(pending))
}

func lastErrorOf(items []spool.Item) string {
	if len(items) == 0 {
		return "none"
	}
	return items[0].LastError
}

// TestRelayKeepsFileAddressedToItself: no destination header means "this is
// for you", which is how the plain Shortcuts setup behaves.
func TestRelayKeepsFileAddressedToItself(t *testing.T) {
	f := newRelayFixture(t)
	resp := f.upload(t, "local.txt", "", []byte("stay here"))
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	pending, _ := f.relaySpool.Pending()
	if len(pending) != 0 {
		t.Errorf("an unaddressed upload was spooled instead of kept")
	}
	if _, err := os.Stat(filepath.Join(f.relay.opts.InboxDir, "local.txt")); err != nil {
		t.Errorf("unaddressed upload did not land in the relay's own inbox: %v", err)
	}
}

// TestRelayRejectsUnknownDestination: spooling for a name that can never be
// resolved would mean holding a file forever while telling the sender it
// was accepted.
func TestRelayRejectsUnknownDestination(t *testing.T) {
	f := newRelayFixture(t)
	resp := f.upload(t, "x.bin", "a-machine-that-was-never-paired", []byte("x"))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
	pending, _ := f.relaySpool.Pending()
	if len(pending) != 0 {
		t.Errorf("file was spooled for an unknown destination")
	}
}

// TestForwarderRefusesToPairWithAnUnknownPeer: a relay has nobody sitting
// in front of it to confirm a code, so accepting a stranger would defeat
// the point of trust-on-first-use.
func TestForwarderRefusesToPairWithAnUnknownPeer(t *testing.T) {
	// A destination that is listening but has never been paired with.
	strangerKnown, err := pairing.NewKnownPeers(t.TempDir(), peer.NewMemStore())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stranger, err := startPeerServer(ctx, peerServerOptions{
		ListenAddr: "127.0.0.1:0",
		InboxDir:   t.TempDir(),
		Known:      strangerKnown,
		Confirmer:  func(a, b [32]byte, n, c string, k bool) bool { return true },
		Logf:       func(string, ...any) {},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer stranger.Close()

	// The relay's store names the peer but has never actually paired with
	// its key, so pairing must fail rather than silently succeed.
	store := peer.NewMemStore()
	store.Add(peer.Peer{Name: "stranger", PubKey: [32]byte{1}, LastIP: "127.0.0.1"})
	known, err := pairing.NewKnownPeers(t.TempDir(), store)
	if err != nil {
		t.Fatal(err)
	}
	sp, err := spool.Open(filepath.Join(t.TempDir(), "spool"))
	if err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(t.TempDir(), "f.bin")
	if err := os.WriteFile(src, []byte("payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	item, err := sp.Add("stranger", "f.bin", bytes.NewReader([]byte("payload")))
	if err != nil {
		t.Fatal(err)
	}

	id, err := pairing.LoadOrCreateIdentity(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	fw := &forwarder{
		spool: sp, known: known, identity: &id,
		port: stranger.Addr().(*net.TCPAddr).Port,
		logf: func(string, ...any) {},
	}
	if err := fw.deliver(item); err == nil {
		t.Error("relay paired with a peer it had never actually met")
	}
	if remaining, _ := sp.Pending(); len(remaining) != 1 {
		t.Errorf("spool holds %d items after a failed delivery, want the file kept", len(remaining))
	}
}

// TestRelayDefaultDestinationForwardsUnaddressedUploads: every header is
// another row to type on a phone keyboard, so a relay whose whole job is
// passing things on should not need to be told that on every request.
func TestRelayDefaultDestinationForwardsUnaddressedUploads(t *testing.T) {
	f := newRelayFixture(t)
	f.relay.opts.Spool = f.relaySpool

	// Rebuild the handler with a default destination, the way
	// `--relay-to laptop` configures it.
	h := uploadHandlerRouted(uploadRoute{
		InboxDir:   f.relay.opts.InboxDir,
		Token:      testToken,
		SelfName:   "the-relay",
		Spool:      f.relaySpool,
		KnownNames: func() []string { return []string{f.laptopName} },
		DefaultTo:  f.laptopName,
		Logf:       func(string, ...any) {},
	})

	// No X-Beamdrop-To at all — it should still be held for the laptop.
	w := postUpload(t, h, "photo bytes", "IMG_1.jpeg", testToken, "100.64.0.2:5000")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body)
	}
	pending, err := f.relaySpool.PendingFor(f.laptopName)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 {
		t.Fatalf("spool holds %d for the laptop, want 1 — the default was ignored", len(pending))
	}

	// "here" still overrides it, so a file can be kept on the relay.
	r := httptest.NewRequest(http.MethodPost, "/upload", strings.NewReader("stay"))
	r.Header.Set("Authorization", "Bearer "+testToken)
	r.Header.Set("X-Beamdrop-Filename", "local.txt")
	r.Header.Set("X-Beamdrop-To", "here")
	r.RemoteAddr = "100.64.0.2:5000"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf(`"here" override: status %d`, rec.Code)
	}
	if again, _ := f.relaySpool.PendingFor(f.laptopName); len(again) != 1 {
		t.Errorf(`"here" was forwarded anyway: spool now holds %d`, len(again))
	}
	if _, err := os.Stat(filepath.Join(f.relay.opts.InboxDir, "local.txt")); err != nil {
		t.Errorf(`"here" did not land in the relay's own inbox: %v`, err)
	}
}
