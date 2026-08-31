package webui

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// serveShim stands up a WebSocket server that hands the server-side shim
// back over a channel, plus a connected client.
func serveShim(t *testing.T) (*wsShim, *websocket.Conn) {
	t.Helper()
	shimCh := make(chan *wsShim, 1)
	upgrader := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		shimCh <- newWSShim(c)
	}))
	t.Cleanup(srv.Close)

	client, _, err := websocket.DefaultDialer.Dial("ws"+srv.URL[len("http"):], nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })

	select {
	case shim := <-shimCh:
		return shim, client
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for the server-side shim")
		return nil, nil
	}
}

// TestShimReadReportsPeerCloseAsEOF: a phone closing its tab is an ordinary
// hang-up. gorilla reports it as *websocket.CloseError, which nothing
// upstream recognises, so the portal logged every closed tab as an error
// ("websocket: close 1006 (abnormal closure)"). The shim's whole job is to
// make a WebSocket look like a net.Conn, and a net.Conn signals a departed
// peer with io.EOF.
func TestShimReadReportsPeerCloseAsEOF(t *testing.T) {
	for _, tc := range []struct {
		name       string
		disconnect func(c *websocket.Conn)
	}{
		{"clean close handshake", func(c *websocket.Conn) {
			_ = c.WriteMessage(websocket.CloseMessage,
				websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
			c.Close()
		}},
		{"abrupt disconnect", func(c *websocket.Conn) {
			// What Safari actually does when the phone locks or the tab is
			// swiped away: the TCP connection drops with no close frame,
			// which gorilla reports as close 1006.
			c.UnderlyingConn().Close()
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			shim, client := serveShim(t)
			tc.disconnect(client)

			errCh := make(chan error, 1)
			go func() {
				_, err := shim.Read(make([]byte, 16))
				errCh <- err
			}()
			select {
			case err := <-errCh:
				if !errors.Is(err, io.EOF) {
					t.Errorf("Read after peer close = %v, want io.EOF", err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("Read did not return after the peer went away")
			}
		})
	}
}

func TestShimReadSkipsTextFrames(t *testing.T) {
	shimCh := make(chan *wsShim, 1)
	upgrader := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		shimCh <- newWSShim(c)
	}))
	defer srv.Close()

	wsURL := "ws" + srv.URL[len("http"):]
	client, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	if err := client.WriteMessage(websocket.TextMessage, []byte("ignore me")); err != nil {
		t.Fatal(err)
	}
	want := []byte("binary payload")
	if err := client.WriteMessage(websocket.BinaryMessage, want); err != nil {
		t.Fatal(err)
	}

	var shim *wsShim
	select {
	case shim = <-shimCh:
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for server-side shim")
	}

	got := make([]byte, len(want))
	done := make(chan error, 1)
	go func() {
		_, err := shim.Read(got)
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Read did not return — likely spinning on the text frame")
	}
	if !bytes.Equal(got, want) {
		t.Errorf("Read = %q, want %q", got, want)
	}
}
