// Package netmux lets beamdrop's two dialects share a single port.
//
// The portal has to be reachable two ways at once: an iPhone loads the web
// UI over HTTPS and upgrades to a WebSocket, while `beamdrop send` and
// `beamdrop watch` speak the raw frame protocol over plain TCP. Asking the
// user to remember two port numbers for one program is a worse answer than
// looking at the first two bytes.
package netmux

import (
	"bytes"
	"io"
	"net"
	"sync"
	"time"
)

// sniffTimeout bounds how long a connection may stay unclassified. It only
// has to cover the network delay between connect and first write, since
// both dialects write immediately: TLS sends a ClientHello, and a beamdrop
// initiator sends HELLO.
const sniffTimeout = 10 * time.Second

// kind is what a connection turned out to be.
type kind int

const (
	kindTLS   kind = iota // the phone's HTTPS session
	kindFrame             // a beamdrop peer speaking the frame protocol
	kindPlain             // something else, in practice a plaintext HTTP request
)

// classify decides from a connection's first two bytes.
//
// One byte is not enough to separate the first two. 0x16 is the TLS
// handshake record type, but it is also a legitimate beamdrop frame length
// — a HELLO carrying a 17-character name is exactly 22 bytes. The second
// byte separates them: for TLS it is the record's major version (0x03),
// while for a HELLO it is the second byte of a little-endian u32 length.
// Engine.pair() truncates names to 32 bytes, capping a HELLO frame at 41
// bytes total, so that byte is always 0x00 — and HELLO is always the first
// frame a peer sends.
//
// That 0x00 is also what rules everything else out. Every HTTP method
// starts with two ASCII letters, so a browser that was handed "host:4747"
// and turned it into http:// lands in kindPlain rather than being fed to
// the frame parser, which used to close the connection with no reply at
// all — a browser reports that as "cannot open the page", which is exactly
// what it looks like when the server is down.
func classify(hdr [2]byte) kind {
	switch {
	case hdr[0] == 0x16 && hdr[1] == 0x03:
		return kindTLS
	case hdr[1] == 0x00:
		return kindFrame
	default:
		return kindPlain
	}
}

// Listener is a net.Listener that yields only the TLS connections arriving
// on the wrapped listener. Everything else is handed to the onRaw callback
// instead, so an http.Server can consume this directly.
type Listener struct {
	inner net.Listener
	onRaw func(net.Conn)

	tlsCh   chan net.Conn
	plainCh chan net.Conn

	mu      sync.Mutex
	done    chan struct{}
	closed  bool
	acceptE error
}

// Listen wraps inner. Connections whose first bytes are not a TLS record
// are passed to onRaw on their own goroutine; onRaw owns closing them.
func Listen(inner net.Listener, onRaw func(net.Conn)) *Listener {
	l := &Listener{
		inner:   inner,
		onRaw:   onRaw,
		tlsCh:   make(chan net.Conn),
		plainCh: make(chan net.Conn),
		done:    make(chan struct{}),
	}
	go l.acceptLoop()
	return l
}

func (l *Listener) acceptLoop() {
	for {
		c, err := l.inner.Accept()
		if err != nil {
			l.shutdown(err)
			return
		}
		// Classify off the accept path. Reading the first bytes blocks
		// until the client writes, so doing it inline would let one
		// connection that opens and says nothing hold up every client
		// behind it.
		go l.classify(c)
	}
}

func (l *Listener) classify(c net.Conn) {
	var hdr [2]byte
	_ = c.SetReadDeadline(time.Now().Add(sniffTimeout))
	if _, err := io.ReadFull(c, hdr[:]); err != nil {
		c.Close()
		return
	}
	_ = c.SetReadDeadline(time.Time{})

	// Whoever handles this connection still needs the bytes we consumed.
	replayed := &prefixConn{Conn: c, r: io.MultiReader(bytes.NewReader(hdr[:]), c)}

	switch classify(hdr) {
	case kindTLS:
		l.handOff(l.tlsCh, replayed)
	case kindPlain:
		l.handOff(l.plainCh, replayed)
	default:
		l.onRaw(replayed)
	}
}

func (l *Listener) handOff(ch chan net.Conn, c net.Conn) {
	select {
	case ch <- c:
	case <-l.done:
		c.Close()
	}
}

func (l *Listener) shutdown(err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return
	}
	l.closed = true
	l.acceptE = err
	close(l.done)
}

// Accept returns the next TLS connection. It reports the underlying
// listener's error once that listener stops accepting, which is what tells
// http.Server to stop serving.
func (l *Listener) Accept() (net.Conn, error) { return l.accept(l.tlsCh) }

// PlainHTTP is a listener for connections that were neither TLS nor a
// beamdrop frame. Serve it with an http.Server that redirects to https:
// the alternative is dropping the connection, which a browser cannot tell
// apart from the server being down.
func (l *Listener) PlainHTTP() net.Listener { return plainListener{l} }

func (l *Listener) accept(ch chan net.Conn) (net.Conn, error) {
	select {
	case c := <-ch:
		return c, nil
	case <-l.done:
		l.mu.Lock()
		defer l.mu.Unlock()
		if l.acceptE != nil {
			return nil, l.acceptE
		}
		return nil, net.ErrClosed
	}
}

// plainListener is a view onto the same Listener that yields the plaintext
// connections. Close is a no-op: the two views share one socket, and an
// http.Server shutting down must not take the TLS side with it.
type plainListener struct{ l *Listener }

func (p plainListener) Accept() (net.Conn, error) { return p.l.accept(p.l.plainCh) }
func (p plainListener) Close() error              { return nil }
func (p plainListener) Addr() net.Addr            { return p.l.Addr() }

// Close stops the listener. It is safe to call more than once — http.Server
// closes its listener on Shutdown, and the portal closes it too.
func (l *Listener) Close() error {
	l.shutdown(nil)
	return l.inner.Close()
}

func (l *Listener) Addr() net.Addr { return l.inner.Addr() }

// prefixConn replays bytes already read off the socket during
// classification, then reads through to the connection.
type prefixConn struct {
	net.Conn
	r io.Reader
}

func (c *prefixConn) Read(p []byte) (int, error) { return c.r.Read(p) }
