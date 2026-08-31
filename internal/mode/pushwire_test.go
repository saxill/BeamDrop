package mode

import (
	"context"
	"encoding/hex"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/saxill/beamdrop/internal/engine"
	"github.com/saxill/beamdrop/internal/frame"
	"github.com/saxill/beamdrop/internal/pairing"
	"github.com/saxill/beamdrop/internal/peer"
	"github.com/saxill/beamdrop/internal/push"
)

// dialPortal brings up a portal with push enabled and returns a paired
// client engine, exercising the same path a phone takes.
func dialPortal(t *testing.T) (*engine.Engine, *push.Store, *peerServer, pairing.KeyPair) {
	t.Helper()

	pushStore, err := push.Open(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	srvKP, err := pairing.LoadOrCreateIdentity(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	known, err := pairing.NewKnownPeers(t.TempDir(), peer.NewMemStore())
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	srv, err := startPeerServer(ctx, peerServerOptions{
		ListenAddr: "127.0.0.1:0",
		InboxDir:   t.TempDir(),
		Known:      known,
		Identity:   &srvKP,
		Push:       pushStore,
		Confirmer:  func([32]byte, [32]byte, string, string, bool) bool { return true },
		Logf:       func(string, ...any) {},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Close() })

	clientKP, err := pairing.LoadOrCreateIdentity(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.DialTimeout("tcp", srv.Addr().String(), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })

	e, err := engine.New(engine.Config{
		Name:        "test-phone",
		Conn:        conn,
		IsInitiator: true,
		Identity:    &clientKP,
		Confirmer:   func([32]byte, [32]byte, string, string, bool) bool { return true },
	})
	if err != nil {
		t.Fatal(err)
	}
	return e, pushStore, srv, clientKP
}

// The page cannot subscribe without the server's VAPID public key, and it
// asks for it over this connection rather than an HTTP endpoint so the
// request inherits the pairing that just happened.
func TestPortalAnswersAPushKeyRequest(t *testing.T) {
	e, store, _, _ := dialPortal(t)

	got := make(chan string, 1)
	e.OnPushKey(func(key string) { got <- key })
	go e.Serve()

	if err := e.RequestPushKey(); err != nil {
		t.Fatal(err)
	}
	select {
	case key := <-got:
		if key == "" {
			t.Fatal("the portal answered with an empty key; the page would never offer notifications")
		}
		if key != store.PublicKey() {
			t.Errorf("key = %q, want the store's %q", key, store.PublicKey())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no answer to the push key request")
	}
}

// The registration must reach the store, tagged with the identity that made
// it — that tag is what stops a phone being told about its own uploads.
func TestPortalStoresAPushSubscription(t *testing.T) {
	e, store, _, clientKP := dialPortal(t)
	go e.Serve()

	sub := frame.PushSubscribePayload{
		Endpoint: "https://push.example.com/abc123",
		P256dh:   "a-key",
		Auth:     "an-auth",
	}
	if err := e.SendPushSubscription(sub); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if all := store.All(); len(all) == 1 {
			got := all[0]
			if got.Endpoint != sub.Endpoint {
				t.Errorf("Endpoint = %q", got.Endpoint)
			}
			if got.Peer != "test-phone" {
				t.Errorf("Peer = %q, want test-phone", got.Peer)
			}
			want := hex.EncodeToString(clientKP.Pub[:])
			if got.PeerKey != want {
				t.Errorf("PeerKey = %q, want the subscribing peer's key %q", got.PeerKey, want)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the subscription never reached the store")
}

// An endpoint is a URL this machine will then make outbound requests to, so
// a peer must not be able to aim it anywhere it likes.
func TestPortalRefusesAHostileSubscription(t *testing.T) {
	for _, endpoint := range []string{
		"http://push.example.com/plain",
		"file:///etc/passwd",
		"gopher://internal.service/x",
		"",
	} {
		t.Run(fmt.Sprintf("%q", endpoint), func(t *testing.T) {
			e, store, _, _ := dialPortal(t)
			go e.Serve()

			// Encoding rejects some of these outright; for the rest the
			// server-side decoder must. Either way nothing may be stored.
			_ = e.SendPushSubscription(frame.PushSubscribePayload{
				Endpoint: endpoint, P256dh: "k", Auth: "a",
			})
			time.Sleep(300 * time.Millisecond)
			if n := len(store.All()); n != 0 {
				t.Errorf("stored %d subscriptions for endpoint %q", n, endpoint)
			}
			// And the connection must still be alive: a bad registration is
			// not worth dropping a working transfer channel over.
			if err := e.SendText("still here"); err != nil {
				t.Errorf("the connection died after a bad subscription: %v", err)
			}
		})
	}
}
