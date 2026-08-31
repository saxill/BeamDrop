package netmux

import (
	"bytes"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"testing"
	"time"
)

// helloFrame is what an initiating beamdrop peer puts on the wire first:
// [len:u32 LE][type:u8][mode:u8][caps:u16][nameLen:u8][name].
func helloFrame(name string) []byte {
	payload := append([]byte{0x05, 0x00, 0x00, byte(len(name))}, name...)
	out := []byte{byte(len(payload) + 1), 0x00, 0x00, 0x00, 0x01}
	return append(out, payload...)
}

func newTestMux(t *testing.T) (*Listener, chan net.Conn) {
	t.Helper()
	inner, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	raw := make(chan net.Conn, 4)
	l := Listen(inner, func(c net.Conn) { raw <- c })
	t.Cleanup(func() { l.Close() })
	return l, raw
}

func TestRawConnectionKeepsItsSniffedBytes(t *testing.T) {
	l, raw := newTestMux(t)

	want := helloFrame("laptop")
	c, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Write(want); err != nil {
		t.Fatal(err)
	}

	var got net.Conn
	select {
	case got = <-raw:
	case <-time.After(5 * time.Second):
		t.Fatal("raw handler never saw the connection")
	}
	defer got.Close()

	// The sniff consumed the first bytes off the socket; the handler has to
	// see them anyway or the engine reads a truncated frame.
	buf := make([]byte, len(want))
	if err := got.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(got, buf); err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(buf, want) {
		t.Errorf("handler read %x, want %x", buf, want)
	}
}

func TestTLSConnectionReachesAccept(t *testing.T) {
	l, raw := newTestMux(t)

	certPEM, keyPEM := testCert(t)
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	srvTLS := tls.NewListener(l, &tls.Config{Certificates: []tls.Certificate{cert}})

	go func() {
		c, err := srvTLS.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		io.Copy(c, c)
	}()

	client, err := tls.Dial("tcp", l.Addr().String(), &tls.Config{InsecureSkipVerify: true})
	if err != nil {
		t.Fatalf("tls dial: %v", err)
	}
	defer client.Close()
	if _, err := client.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4)
	if err := client.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(client, buf); err != nil {
		t.Fatalf("read echo: %v", err)
	}
	if string(buf) != "ping" {
		t.Errorf("echo = %q, want %q", buf, "ping")
	}
	select {
	case <-raw:
		t.Error("a TLS connection was routed to the raw handler")
	default:
	}
}

// TestFrameLengthCollidingWithTLSRecordTypeIsRoutedRaw is the reason the
// sniff reads two bytes instead of one. 0x16 is the TLS handshake record
// type, but it is also a perfectly ordinary beamdrop frame length (22
// bytes) — a HELLO carrying a 17-character name hits it exactly. The second
// byte disambiguates: it is the TLS major version (0x03) against the second
// byte of a little-endian u32 length, which is always 0x00 for a HELLO
// (pair() caps names at 32 bytes, so the frame cannot reach 256).
func TestFrameLengthCollidingWithTLSRecordTypeIsRoutedRaw(t *testing.T) {
	l, raw := newTestMux(t)

	want := helloFrame("seventeen-chars17")
	if want[0] != 0x16 {
		t.Fatalf("test setup: first byte is %#x, want 0x16", want[0])
	}

	c, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Write(want); err != nil {
		t.Fatal(err)
	}

	select {
	case got := <-raw:
		defer got.Close()
		buf := make([]byte, len(want))
		got.SetReadDeadline(time.Now().Add(5 * time.Second))
		if _, err := io.ReadFull(got, buf); err != nil {
			t.Fatalf("read: %v", err)
		}
		if !bytes.Equal(buf, want) {
			t.Errorf("handler read %x, want %x", buf, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a beamdrop frame of length 0x16 was misrouted as TLS")
	}
}

// TestSilentConnectionDoesNotStallOtherClients pins the reason
// classification happens per connection rather than inline in Accept: a
// client that opens a socket and never writes would otherwise hold up
// every other client behind it.
func TestSilentConnectionDoesNotStallOtherClients(t *testing.T) {
	l, raw := newTestMux(t)

	silent, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer silent.Close()

	talker, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer talker.Close()
	if _, err := talker.Write(helloFrame("laptop")); err != nil {
		t.Fatal(err)
	}

	select {
	case c := <-raw:
		c.Close()
	case <-time.After(5 * time.Second):
		t.Fatal("a silent connection blocked classification of a later one")
	}
}

func TestCloseUnblocksAccept(t *testing.T) {
	l, _ := newTestMux(t)

	done := make(chan error, 1)
	go func() {
		_, err := l.Accept()
		done <- err
	}()
	time.Sleep(50 * time.Millisecond)
	l.Close()

	select {
	case err := <-done:
		if err == nil {
			t.Error("Accept returned nil error after Close")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Accept did not return after Close")
	}
}

// TestPlaintextHTTPGetsItsOwnListener covers the address-bar case: typing
// "host:4747" into a browser produces http://, not https://. Feeding that
// to the frame parser closed the connection with zero bytes back, which a
// browser reports as "cannot open the page" — identical to the server being
// down, and the user has no way to tell the difference.
func TestPlaintextHTTPGetsItsOwnListener(t *testing.T) {
	l, raw := newTestMux(t)

	served := make(chan string, 1)
	go func() {
		srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			served <- r.Host
			w.WriteHeader(http.StatusTeapot)
		})}
		_ = srv.Serve(l.PlainHTTP())
	}()

	resp, err := (&http.Client{Timeout: 5 * time.Second}).Get("http://" + l.Addr().String() + "/")
	if err != nil {
		t.Fatalf("plain http request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTeapot {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusTeapot)
	}
	select {
	case <-served:
	case <-time.After(2 * time.Second):
		t.Fatal("the HTTP handler never saw the request")
	}
	select {
	case <-raw:
		t.Error("a plaintext HTTP request was fed to the frame parser")
	default:
	}
}
