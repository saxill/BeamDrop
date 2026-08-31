package discovery

import (
	"context"
	"net"
	"strconv"
	"testing"
	"time"
)

func startResponder(t *testing.T, self Self) int {
	t.Helper()
	r, err := Listen("127.0.0.1:0", self)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	return r.Addr().(*net.UDPAddr).Port
}

func TestFindLocatesAPortalThatNeverAdvertisedItself(t *testing.T) {
	self := Self{Name: "laptop", PubKey: [32]byte{1, 2, 3}, Port: 4747}
	port := startResponder(t, self)

	start := time.Now()
	peers, err := Find(context.Background(), FindOptions{
		Port:      port,
		Timeout:   5 * time.Second,
		Targets:   []string{"127.0.0.1"},
		StopAfter: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(peers) != 1 {
		t.Fatalf("found %d peers, want 1", len(peers))
	}
	// StopAfter means a one-shot send does not pay the full timeout just to
	// learn there is no second laptop.
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("Find took %v after already having its answer", elapsed)
	}
	if peers[0].Name != "laptop" {
		t.Errorf("name = %q, want %q", peers[0].Name, "laptop")
	}
	if peers[0].PubKey != self.PubKey {
		t.Error("public key does not match the responder's")
	}
	// The address has to be dialable as-is: the host comes from the reply's
	// source address, the port from the payload.
	host, p, err := net.SplitHostPort(peers[0].Addr)
	if err != nil {
		t.Fatalf("Addr %q is not host:port: %v", peers[0].Addr, err)
	}
	if host != "127.0.0.1" {
		t.Errorf("host = %q, want 127.0.0.1", host)
	}
	if p != strconv.Itoa(self.Port) {
		t.Errorf("port = %q, want %d — the TCP port, not the UDP one it replied from", p, self.Port)
	}
}

func TestFindReturnsNothingWhenNoPortalIsRunning(t *testing.T) {
	// Bind and immediately release a port so we know nothing is on it.
	c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	port := c.LocalAddr().(*net.UDPAddr).Port
	c.Close()

	start := time.Now()
	peers, err := Find(context.Background(), FindOptions{
		Port: port, Timeout: 300 * time.Millisecond, Targets: []string{"127.0.0.1"},
	})
	if err != nil {
		t.Fatalf("silence should not be an error: %v", err)
	}
	if len(peers) != 0 {
		t.Errorf("found %d peers, want none", len(peers))
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("Find took %v, well past its timeout", elapsed)
	}
}

// TestResponderIgnoresGarbage: the port is reachable by anything on the
// network, so a stray datagram must not take the responder down or draw a
// reply that leaks the machine's name and public key.
func TestResponderIgnoresGarbage(t *testing.T) {
	self := Self{Name: "laptop", PubKey: [32]byte{9}, Port: 4747}
	port := startResponder(t, self)

	junk, err := net.DialUDP("udp4", nil, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port})
	if err != nil {
		t.Fatal(err)
	}
	defer junk.Close()
	for _, b := range [][]byte{
		{},
		[]byte("hello"),
		append([]byte("BDRP"), 99, 1, 0, 0), // right magic, wrong version
		make([]byte, 400),                   // zeros
	} {
		if _, err := junk.Write(b); err != nil {
			t.Fatal(err)
		}
	}
	// Nothing should come back.
	_ = junk.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	if n, err := junk.Read(make([]byte, 512)); err == nil {
		t.Errorf("responder replied to garbage with %d bytes", n)
	}

	// And it is still working afterwards.
	peers, err := Find(context.Background(), FindOptions{
		Port: port, Timeout: 2 * time.Second, Targets: []string{"127.0.0.1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(peers) != 1 {
		t.Fatalf("responder stopped answering after garbage: found %d peers", len(peers))
	}
}

func TestCodecRoundTrip(t *testing.T) {
	want := Self{Name: "a-fairly-long-hostname-here", PubKey: [32]byte{1, 255, 7}, Port: 65000}
	kind, got, err := decode(encode(kindAnnounce, want))
	if err != nil {
		t.Fatal(err)
	}
	if kind != kindAnnounce {
		t.Errorf("kind = %d, want %d", kind, kindAnnounce)
	}
	if got != want {
		t.Errorf("round trip = %+v, want %+v", got, want)
	}
}

func TestEncodeTruncatesOverlongNames(t *testing.T) {
	// nameLen is a single byte and the spec caps HELLO names at 32; keeping
	// the same ceiling here means a long hostname cannot produce a datagram
	// the decoder rejects.
	long := "this-hostname-is-considerably-longer-than-thirty-two-bytes"
	_, got, err := decode(encode(kindAnnounce, Self{Name: long, Port: 1}))
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Name) != 32 {
		t.Errorf("name length = %d, want 32", len(got.Name))
	}
}

func TestDecodeRejectsTruncatedDatagram(t *testing.T) {
	full := encode(kindAnnounce, Self{Name: "x", Port: 1})
	for _, n := range []int{0, 1, 4, headerLen - 1, headerLen} {
		if _, _, err := decode(full[:n]); err == nil && n < len(full) {
			t.Errorf("decode accepted a %d-byte datagram", n)
		}
	}
}
