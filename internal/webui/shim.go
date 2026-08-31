package webui

import (
	"errors"
	"io"
	"net"
	"time"

	"github.com/gorilla/websocket"
)

// wsShim adapts a gorilla websocket to a net.Conn so the engine can treat it
// like a TCP connection. It satisfies the full net.Conn interface: LocalAddr /
// RemoteAddr return the websocket's peer addresses, and the deadline methods
// are no-ops (the websocket library has its own read deadline configuration).
type wsShim struct {
	c   *websocket.Conn
	bin []byte
}

func newWSShim(c *websocket.Conn) *wsShim {
	return &wsShim{c: c}
}

func (w *wsShim) Read(p []byte) (int, error) {
	for len(w.bin) == 0 {
		t, data, err := w.c.ReadMessage()
		if err != nil {
			return 0, translateClose(err)
		}
		if t != websocket.BinaryMessage {
			continue // skip text/control frames; wait for a binary message
		}
		w.bin = data
	}
	n := copy(p, w.bin)
	w.bin = w.bin[n:]
	return n, nil
}

// translateClose turns "the peer went away" into io.EOF.
//
// This shim exists to make a WebSocket look like a net.Conn, and a net.Conn
// reports a departed peer as io.EOF. gorilla instead returns a
// *websocket.CloseError, which nothing upstream recognises — so every phone
// that locked its screen or closed the tab showed up in the portal log as
// "websocket: close 1006 (abnormal closure)", styled as a failure. A phone
// disconnecting is the most ordinary thing that happens here.
func translateClose(err error) error {
	var ce *websocket.CloseError
	if errors.As(err, &ce) {
		return io.EOF
	}
	return err
}

func (w *wsShim) Write(p []byte) (int, error) {
	if err := w.c.WriteMessage(websocket.BinaryMessage, p); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (w *wsShim) Close() error { return w.c.Close() }

// LocalAddr returns a synthetic address derived from the websocket's
// underlying network connection.
func (w *wsShim) LocalAddr() net.Addr {
	if a := w.c.LocalAddr(); a != nil {
		return a
	}
	return &net.TCPAddr{}
}

// RemoteAddr returns a synthetic address derived from the websocket's
// underlying network connection.
func (w *wsShim) RemoteAddr() net.Addr {
	if a := w.c.RemoteAddr(); a != nil {
		return a
	}
	return &net.TCPAddr{}
}

// SetDeadline is a no-op; gorilla websocket has its own deadline APIs.
func (w *wsShim) SetDeadline(t time.Time) error { return nil }

// SetReadDeadline is a no-op; gorilla websocket has its own deadline APIs.
func (w *wsShim) SetReadDeadline(t time.Time) error { return nil }

// SetWriteDeadline is a no-op; gorilla websocket has its own deadline APIs.
func (w *wsShim) SetWriteDeadline(t time.Time) error { return nil }
