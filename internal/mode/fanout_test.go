package mode

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/saxill/beamdrop/internal/engine"
	"github.com/saxill/beamdrop/internal/pairing"
	"github.com/saxill/beamdrop/internal/peer"
	"github.com/saxill/beamdrop/internal/spool"
)

// newFanOutServer builds just enough peerServer to exercise relay. The
// method takes the sender's name, a send closure and a queue closure, so
// none of this needs a live connection.
func newFanOutServer(t *testing.T, sp *spool.Spool, defaultTo string) *peerServer {
	t.Helper()
	var logged []string
	return &peerServer{
		opts: peerServerOptions{
			Spool:     sp,
			DefaultTo: defaultTo,
			Logf:      func(f string, a ...any) { logged = append(logged, f) },
		},
		messages: newMessageLog(),
		registry: engine.NewRegistry(),
	}
}

// noopSend is the send half of relay for tests that only exercise the queue
// path (a message from a phone), where the send closure is never called.
func noopSend(*engine.Engine) error { return nil }

func openSpool(t *testing.T) *spool.Spool {
	t.Helper()
	sp, err := spool.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return sp
}

func pending(t *testing.T, sp *spool.Spool) []spool.Item {
	t.Helper()
	items, err := sp.Pending()
	if err != nil {
		t.Fatal(err)
	}
	return items
}

// The point of the whole feature: a phone sending to the relay reaches the
// laptop, instead of the file stopping at the relay's own inbox.
func TestFanOutQueuesForTheDefaultDestination(t *testing.T) {
	sp := openSpool(t)
	s := newFanOutServer(t, sp, "laptop")

	s.relay("iPhone", noopSend, func(to string) error {
		_, err := sp.AddText(to, "hello from the phone")
		return err
	})

	items := pending(t, sp)
	if len(items) != 1 {
		t.Fatalf("got %d spooled items, want 1", len(items))
	}
	if items[0].To != "laptop" {
		t.Errorf("queued for %q, want laptop", items[0].To)
	}
	body, err := sp.Text(items[0])
	if err != nil {
		t.Fatal(err)
	}
	if body != "hello from the phone" {
		t.Errorf("body = %q", body)
	}
}

// The loop. On a relay whose entire job is retrying until delivery
// succeeds, sending the destination's own message back to it does not
// settle down — it fills a disk.
func TestFanOutIgnoresThingsFromTheDestinationItself(t *testing.T) {
	for _, from := range []string{"laptop", "LAPTOP", "Laptop"} {
		t.Run(from, func(t *testing.T) {
			sp := openSpool(t)
			s := newFanOutServer(t, sp, "laptop")

			s.relay(from, noopSend, func(to string) error {
				_, err := sp.AddText(to, "should never be queued")
				return err
			})

			if items := pending(t, sp); len(items) != 0 {
				t.Errorf("a message from the destination %q was queued back to it (%d items)",
					from, len(items))
			}
		})
	}
}

// The reverse half of the relay: a message that arrived *from* the
// destination (the laptop dialling in) is broadcast to every other connected
// peer (the phone) rather than spooled back to the laptop.
func TestRelayBroadcastsFromTheDestinationToConnectedPeers(t *testing.T) {
	sp := openSpool(t)
	s := newFanOutServer(t, sp, "laptop")

	phone, cleanup := pairedTestEngine(t, t.TempDir())
	defer cleanup()
	s.registry.Add(phone)

	var sentTo []string
	queued := false
	s.relay("laptop", func(e *engine.Engine) error {
		sentTo = append(sentTo, e.PeerName())
		return nil
	}, func(to string) error {
		queued = true
		return nil
	})

	if len(sentTo) != 1 || sentTo[0] != "responder" {
		t.Errorf("broadcast to %v, want [responder]", sentTo)
	}
	if queued {
		t.Error("a message from the destination was spooled back to it")
	}
	if items := pending(t, sp); len(items) != 0 {
		t.Errorf("spooled %d items", len(items))
	}
}

// A plain portal is not a relay and has nowhere to pass anything on to.
func TestFanOutDoesNothingWithoutARelay(t *testing.T) {
	t.Run("no spool", func(t *testing.T) {
		s := newFanOutServer(t, nil, "laptop")
		called := false
		s.relay("iPhone", noopSend, func(string) error { called = true; return nil })
		if called {
			t.Error("queued something with no spool configured")
		}
	})
	t.Run("no destination", func(t *testing.T) {
		sp := openSpool(t)
		s := newFanOutServer(t, sp, "")
		s.relay("iPhone", noopSend, func(to string) error {
			_, err := sp.AddText(to, "x")
			return err
		})
		if items := pending(t, sp); len(items) != 0 {
			t.Errorf("queued %d items with no --relay-to set", len(items))
		}
	})
}

// The file is already safely in this machine's inbox by the time fanOut
// runs, so a spool failure must not escalate into anything worse.
func TestFanOutSurvivesAQueueFailure(t *testing.T) {
	sp := openSpool(t)
	s := newFanOutServer(t, sp, "laptop")
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("fanOut panicked on a queue error: %v", r)
		}
	}()
	s.relay("iPhone", noopSend, func(string) error { return os.ErrPermission })
}

// textSink is a destination that does nothing but record the TEXT frames
// sent to it — enough to prove a spooled message arrives as a message.
type textSink struct {
	name string
	port int
}

func startTextSink(t *testing.T) (textSink, *pairing.KnownPeers, *pairing.KeyPair, chan string) {
	t.Helper()

	dstKP, err := pairing.LoadOrCreateIdentity(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	relayKP, err := pairing.LoadOrCreateIdentity(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	port := ln.Addr().(*net.TCPAddr).Port
	sink := textSink{name: "laptop-sink", port: port}

	// The relay refuses to pair with a stranger, so it has to already know
	// the destination — and at an address it can dial.
	store := peer.NewMemStore()
	store.Add(peer.Peer{Name: sink.name, PubKey: dstKP.Pub, LastIP: "127.0.0.1"})
	relayKnown, err := pairing.NewKnownPeers(t.TempDir(), store)
	if err != nil {
		t.Fatal(err)
	}

	got := make(chan string, 4)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				e, err := engine.New(engine.Config{
					Name:        sink.name,
					Conn:        conn,
					IsInitiator: false,
					Identity:    &dstKP,
					Confirmer:   func([32]byte, [32]byte, string, string, bool) bool { return true },
				})
				if err != nil {
					return
				}
				e.OnText(func(body string) { got <- body })
				_ = e.Serve()
			}()
		}
	}()
	return sink, relayKnown, &relayKP, got
}

// End to end through the real forwarder: a spooled message is delivered to
// a listening peer as a TEXT frame, not as a file called "message".
func TestForwarderDeliversASpooledMessage(t *testing.T) {
	dst, known, identity, got := startTextSink(t)

	sp := openSpool(t)
	if _, err := sp.AddText(dst.name, "sent while the laptop was shut"); err != nil {
		t.Fatal(err)
	}

	f := &forwarder{
		spool:    sp,
		known:    known,
		identity: identity,
		port:     dst.port,
		interval: 20 * time.Millisecond,
		logf:     func(string, ...any) {},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	go f.Run(ctx)

	select {
	case body := <-got:
		if body != "sent while the laptop was shut" {
			t.Errorf("delivered %q", body)
		}
	case <-ctx.Done():
		t.Fatal("the message was never delivered")
	}

	// And it must leave the spool, or it would be redelivered forever.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if len(pending(t, sp)) == 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Error("the message stayed in the spool after being delivered")
}

// The regression that made a laptop dialling a relay lose it: the forwarder
// used to dial the destination for every spooled message, and that second
// connection — same public key — evicted the persistent one from the
// registry. It must instead deliver over a connection the destination
// already has open. The known store here has no address for the peer, so a
// dial would fail outright: delivery can only have happened over the live
// engine.
func TestForwarderDeliversOverAnExistingConnection(t *testing.T) {
	recvDir := t.TempDir()
	if err := os.MkdirAll(recvDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// A live pair. The initiator is the "connected peer" the relay already
	// has in its registry; the responder is the far end that must actually
	// receive the text.
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
	got := make(chan string, 1)
	respE.OnText(func(body string) { got <- body })
	go respE.Serve()
	res := <-initCh
	if res.err != nil {
		t.Fatal(res.err)
	}
	initE := res.e
	defer func() {
		res.conn.Close()
		respConn.Close()
	}()

	// The relay's registry already holds the connected peer (the initiator,
	// whose PeerName is "responder").
	reg := engine.NewRegistry()
	reg.Add(initE)

	// A known store with nothing in it: addrFor("responder") would error, so
	// the only way the message arrives is via connectedEngine.
	known, err := pairing.NewKnownPeers(t.TempDir(), peer.NewMemStore())
	if err != nil {
		t.Fatal(err)
	}

	sp := openSpool(t)
	if _, err := sp.AddText("responder", "over the live connection"); err != nil {
		t.Fatal(err)
	}

	f := &forwarder{
		spool:    sp,
		known:    known,
		registry: reg,
		interval: 20 * time.Millisecond,
		logf:     func(string, ...any) {},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	go f.Run(ctx)

	select {
	case body := <-got:
		if body != "over the live connection" {
			t.Errorf("delivered %q", body)
		}
	case <-ctx.Done():
		t.Fatal("the message was never delivered over the existing connection")
	}

	// And it must leave the spool, or it would be redelivered forever.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if len(pending(t, sp)) == 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Error("the message stayed in the spool after being delivered")
}

// A message spooled before Kind existed has no "kind" key at all; it must
// still be treated as a file rather than silently becoming a message.
func TestSpoolItemWithoutKindIsAFile(t *testing.T) {
	dir := t.TempDir()
	sp, err := spool.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sp.Add("laptop", "photo.jpg", strings.NewReader("bytes")); err != nil {
		t.Fatal(err)
	}
	items := pending(t, sp)
	if len(items) != 1 {
		t.Fatalf("got %d items", len(items))
	}
	if items[0].Kind != spool.KindFile {
		t.Errorf("Kind = %q, want the file zero value", items[0].Kind)
	}
	// The on-disk form must not carry a kind key for a file, so an older
	// beamdrop reading this spool sees exactly what it expects.
	metas, _ := filepath.Glob(filepath.Join(dir, "*.json"))
	if len(metas) != 1 {
		t.Fatalf("got %d metadata files", len(metas))
	}
	b, err := os.ReadFile(metas[0])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), `"kind"`) {
		t.Errorf("a file's metadata carries a kind key:\n%s", b)
	}
}
