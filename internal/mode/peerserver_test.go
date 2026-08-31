package mode

import (
	"bytes"
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/saxill/beamdrop/internal/discovery"
	"github.com/saxill/beamdrop/internal/engine"
	"github.com/saxill/beamdrop/internal/pairing"
	"github.com/saxill/beamdrop/internal/peer"
	"github.com/saxill/beamdrop/internal/transfer"
)

// TestPeerServerServesRawPeersAndHTTPSOnOnePort is the test that says
// `beamdrop send` can actually reach a running portal. Before the portal
// grew a raw listener nothing in the product ever called net.Listen for the
// frame protocol, so send and watch — which both dial TCP :4747 — could
// only ever fail with connection refused.
//
// It also pins the sharing: the phone's HTTPS session and a CLI peer have
// to coexist on the one port the user was told about.
func TestPeerServerServesRawPeersAndHTTPSOnOnePort(t *testing.T) {
	inbox := t.TempDir()
	known, err := pairing.NewKnownPeers(t.TempDir(), peer.NewMemStore())
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	srv, err := startPeerServer(ctx, peerServerOptions{
		ListenAddr: "127.0.0.1:0",
		InboxDir:   inbox,
		Known:      known,
		Confirmer:  func(a, b [32]byte, name, code string, known bool) bool { return true },
		Logf:       func(string, ...any) {},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	addr := srv.Addr().String()

	t.Run("https", func(t *testing.T) {
		client := &http.Client{
			Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}},
			Timeout:   5 * time.Second,
		}
		resp, err := client.Get("https://" + addr + "/")
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET / = %d, want 200", resp.StatusCode)
		}
		if !bytes.Contains(body, []byte("app.js")) {
			t.Error("GET / did not serve the beamdrop page")
		}
	})

	t.Run("raw peer sends a file", func(t *testing.T) {
		src := filepath.Join(t.TempDir(), "from-cli.bin")
		writeTestFile(t, src, 40_000)

		conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer conn.Close()

		e, err := engine.New(engine.Config{
			Name:        "cli",
			Conn:        conn,
			IsInitiator: true,
			Confirmer:   func(a, b [32]byte, name, code string, known bool) bool { return true },
		})
		if err != nil {
			t.Fatalf("pair over raw tcp: %v", err)
		}
		s, err := transfer.NewSender(src)
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		if err := e.SendFile(s, nil); err != nil {
			t.Fatalf("send: %v", err)
		}

		assertFileArrived(t, filepath.Join(inbox, "from-cli.bin"), 40_000)

		if got := len(srv.Registry().All()); got != 1 {
			t.Errorf("registry holds %d engines, want 1", got)
		}
	})
}

// TestPeerServerRemovesEnginesOnDisconnect keeps :send fan-out honest — a
// registry that accumulates dead engines would make every later send report
// failures against peers that hung up long ago.
func TestPeerServerRemovesEnginesOnDisconnect(t *testing.T) {
	known, err := pairing.NewKnownPeers(t.TempDir(), peer.NewMemStore())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	srv, err := startPeerServer(ctx, peerServerOptions{
		ListenAddr: "127.0.0.1:0",
		InboxDir:   t.TempDir(),
		Known:      known,
		Confirmer:  func(a, b [32]byte, name, code string, known bool) bool { return true },
		Logf:       func(string, ...any) {},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	conn, err := net.DialTimeout("tcp", srv.Addr().String(), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.New(engine.Config{
		Name: "cli", Conn: conn, IsInitiator: true,
		Confirmer: func(a, b [32]byte, name, code string, known bool) bool { return true },
	}); err != nil {
		t.Fatalf("pair: %v", err)
	}
	waitForRegistrySize(t, srv, 1)

	conn.Close()
	waitForRegistrySize(t, srv, 0)
}

// TestPeerServerCreatesMissingInbox: the default inbox is ~/Portal/inbox,
// which does not exist on a fresh machine, and NewReceiver's O_CREATE does
// not create parent directories — so the first file a phone ever sends
// would fail on an untouched install.
func TestPeerServerCreatesMissingInbox(t *testing.T) {
	inbox := filepath.Join(t.TempDir(), "Portal", "inbox")
	known, err := pairing.NewKnownPeers(t.TempDir(), peer.NewMemStore())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	srv, err := startPeerServer(ctx, peerServerOptions{
		ListenAddr: "127.0.0.1:0",
		InboxDir:   inbox,
		Known:      known,
		Confirmer:  func(a, b [32]byte, name, code string, known bool) bool { return true },
		Logf:       func(string, ...any) {},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	info, err := os.Stat(inbox)
	if err != nil || !info.IsDir() {
		t.Fatalf("inbox %s was not created: %v", inbox, err)
	}
}

// TestPeerServerRecognisesAReturningPeer is what makes the pairing prompt
// bearable. Both sides used to mint a fresh keypair per connection, so the
// known-peers store could never match and every single `beamdrop send`
// asked the human to confirm a code again. This pins both halves: the
// portal recognises a returning peer, and the portal's own key stays put so
// the peer could recognise it back.
func TestPeerServerRecognisesAReturningPeer(t *testing.T) {
	known, err := pairing.NewKnownPeers(t.TempDir(), peer.NewMemStore())
	if err != nil {
		t.Fatal(err)
	}
	portalIdentity, err := pairing.LoadOrCreateIdentity(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var sawKnown []bool

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv, err := startPeerServer(ctx, peerServerOptions{
		ListenAddr: "127.0.0.1:0",
		InboxDir:   t.TempDir(),
		Known:      known,
		Identity:   &portalIdentity,
		Confirmer: func(a, b [32]byte, name, code string, isKnown bool) bool {
			mu.Lock()
			sawKnown = append(sawKnown, isKnown)
			mu.Unlock()
			return true
		},
		Logf: func(string, ...any) {},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	cliIdentity, err := pairing.LoadOrCreateIdentity(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	var portalKeys [][32]byte
	for i := 0; i < 2; i++ {
		conn, err := net.DialTimeout("tcp", srv.Addr().String(), 5*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		e, err := engine.New(engine.Config{
			Name: "cli", Conn: conn, IsInitiator: true,
			Identity:  &cliIdentity,
			Confirmer: func(a, b [32]byte, name, code string, known bool) bool { return true },
		})
		if err != nil {
			conn.Close()
			t.Fatalf("connection %d: %v", i+1, err)
		}
		portalKeys = append(portalKeys, e.PeerPubKey())
		conn.Close()
		waitForRegistrySize(t, srv, 0)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(sawKnown) != 2 {
		t.Fatalf("confirmer ran %d times, want 2", len(sawKnown))
	}
	if sawKnown[0] {
		t.Error("first connection reported known=true before anything was remembered")
	}
	if !sawKnown[1] {
		t.Error("second connection from the same identity was not recognised — the user would re-confirm a code every time")
	}
	if portalKeys[0] != portalKeys[1] {
		t.Error("the portal presented a different public key on the second connection")
	}
}

func waitForRegistrySize(t *testing.T, srv *peerServer, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if len(srv.Registry().All()) == want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("registry holds %d engines, want %d", len(srv.Registry().All()), want)
}

// TestPeerServerIsDiscoverable is the point of the discovery package: a
// `beamdrop send` on the same network should not need to be told an
// address. Probing 127.0.0.1 rather than broadcasting keeps the test off
// the real network — the broadcast fan-out is discovery's own concern.
func TestPeerServerIsDiscoverable(t *testing.T) {
	known, err := pairing.NewKnownPeers(t.TempDir(), peer.NewMemStore())
	if err != nil {
		t.Fatal(err)
	}
	identity, err := pairing.LoadOrCreateIdentity(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	srv, err := startPeerServer(ctx, peerServerOptions{
		ListenAddr: "127.0.0.1:0",
		InboxDir:   t.TempDir(),
		Known:      known,
		Identity:   &identity,
		Confirmer:  func(a, b [32]byte, name, code string, known bool) bool { return true },
		Logf:       func(string, ...any) {},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	port := srv.Addr().(*net.TCPAddr).Port
	peers, err := discovery.Find(ctx, discovery.FindOptions{
		Port: port, Timeout: 3 * time.Second, Targets: []string{"127.0.0.1"}, StopAfter: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(peers) != 1 {
		t.Fatalf("a running portal answered %d probes, want 1", len(peers))
	}
	if peers[0].PubKey != identity.Pub {
		t.Error("the portal announced a key that is not its identity")
	}

	// The announced address has to actually be the beamdrop port, not the
	// UDP socket it happened to reply from.
	conn, err := net.DialTimeout("tcp", peers[0].Addr, 5*time.Second)
	if err != nil {
		t.Fatalf("dial the discovered address %s: %v", peers[0].Addr, err)
	}
	defer conn.Close()
	if _, err := engine.New(engine.Config{
		Name: "cli", Conn: conn, IsInitiator: true,
		Confirmer: func(a, b [32]byte, name, code string, known bool) bool { return true },
	}); err != nil {
		t.Fatalf("pair over the discovered address: %v", err)
	}
}

// TestPeerServerRedirectsPlainHTTPToHTTPS: typing "host:4747" into a
// browser gives http://, not https://. That used to reach the frame parser
// and get the connection dropped with no reply, which Safari reports as
// "cannot open the page" — the user cannot tell that apart from the portal
// not running.
func TestPeerServerRedirectsPlainHTTPToHTTPS(t *testing.T) {
	known, err := pairing.NewKnownPeers(t.TempDir(), peer.NewMemStore())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv, err := startPeerServer(ctx, peerServerOptions{
		ListenAddr: "127.0.0.1:0",
		InboxDir:   t.TempDir(),
		Known:      known,
		Confirmer:  func(a, b [32]byte, name, code string, known bool) bool { return true },
		Logf:       func(string, ...any) {},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	client := &http.Client{
		Timeout:       5 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Get("http://" + srv.Addr().String() + "/")
	if err != nil {
		t.Fatalf("plain http request was dropped instead of answered: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusPermanentRedirect {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusPermanentRedirect)
	}
	if got, want := resp.Header.Get("Location"), "https://"+srv.Addr().String()+"/"; got != want {
		t.Errorf("Location = %q, want %q", got, want)
	}
}
