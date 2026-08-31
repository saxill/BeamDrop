package webui

import (
	"bytes"
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestServeOnConnectReceivesContextAndConn(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()

	var mu sync.Mutex
	var gotCtx context.Context
	var gotConn net.Conn
	done := make(chan struct{})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go Serve(ctx, ServeOptions{
		Port:      port,
		StaticDir: t.TempDir(),
		OnConnect: func(c context.Context, conn net.Conn) error {
			mu.Lock()
			gotCtx, gotConn = c, conn
			mu.Unlock()
			close(done)
			return nil
		},
	})

	dialer := websocket.Dialer{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}
	var wsConn *websocket.Conn
	for i := 0; i < 20; i++ {
		wsConn, _, err = dialer.Dial("wss://127.0.0.1:"+itoa(port)+"/ws", nil)
		if err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer wsConn.Close()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("OnConnect was not called")
	}
	mu.Lock()
	defer mu.Unlock()
	if gotCtx == nil {
		t.Error("OnConnect received nil context")
	}
	if gotConn == nil {
		t.Error("OnConnect received nil conn")
	}
}

// TestServeUsesEmbeddedAssetsWhenStaticDirIsUnset pins the property that
// makes beamdrop a single self-contained binary: with no StaticDir set the
// page has to come out of the executable, not off disk relative to
// whatever directory the user happened to run from.
func TestServeUsesEmbeddedAssetsWhenStaticDirIsUnset(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go Serve(ctx, ServeOptions{Port: port}) // StaticDir deliberately empty

	client := &http.Client{
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}},
		Timeout:   2 * time.Second,
	}
	base := "https://127.0.0.1:" + itoa(port)

	var resp *http.Response
	for i := 0; i < 20; i++ {
		resp, err = client.Get(base + "/")
		if err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("get /: %v", err)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET / = %d, want 200", resp.StatusCode)
	}
	if !bytes.Contains(body, []byte("app.js")) {
		t.Errorf("GET / did not serve the beamdrop page (got %d bytes)", len(body))
	}

	// The nested vendor bundle is the asset most likely to be missed by a
	// too-narrow embed pattern, and without it pairing cannot run at all.
	for _, path := range []string{"/app.js", "/protocol.js", "/transfer.js", "/pairing.js", "/pairing-protocol.js", "/vendor/beamdrop-crypto.mjs"} {
		resp, err := client.Get(base + path)
		if err != nil {
			t.Errorf("GET %s: %v", path, err)
			continue
		}
		n, _ := io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET %s = %d, want 200", path, resp.StatusCode)
			continue
		}
		if n == 0 {
			t.Errorf("GET %s served an empty body", path)
		}
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// TestServeDeliversThePWAAssets: "Add to Home Screen" is silently useless
// without these — iOS falls back to a screenshot for the icon and opens in
// a browser chrome instead of standalone.
func TestServeDeliversThePWAAssets(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go Serve(ctx, ServeOptions{Port: port})

	client := &http.Client{
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}},
		Timeout:   3 * time.Second,
	}
	base := "https://127.0.0.1:" + itoa(port)
	for i := 0; i < 20; i++ {
		if _, err := client.Get(base + "/"); err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	for _, tc := range []struct{ path, wantType string }{
		{"/manifest.webmanifest", "manifest"},
		{"/sw.js", "javascript"},
		{"/icons/apple-touch-icon.png", "image/png"},
		{"/icons/icon-192.png", "image/png"},
		{"/icons/icon-512.png", "image/png"},
	} {
		resp, err := client.Get(base + tc.path)
		if err != nil {
			t.Errorf("GET %s: %v", tc.path, err)
			continue
		}
		n, _ := io.Copy(io.Discard, resp.Body)
		ct := resp.Header.Get("Content-Type")
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET %s = %d, want 200", tc.path, resp.StatusCode)
			continue
		}
		if n == 0 {
			t.Errorf("GET %s served an empty body", tc.path)
		}
		if !strings.Contains(ct, tc.wantType) {
			// A manifest served as text/plain is ignored by the browser, so
			// the type matters as much as the bytes.
			t.Errorf("GET %s Content-Type = %q, want something containing %q", tc.path, ct, tc.wantType)
		}
	}
}

// The https URL is the one the portal prints for the phone, so a route
// mounted for uploads has to answer there — it used to 404, and an upload
// that 404s looks from the laptop exactly like one that was never sent.
func TestServeMountsExtraRoutesBesideThePage(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go Serve(ctx, ServeOptions{
		Port: port,
		Routes: map[string]http.Handler{
			"/upload": http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				io.Copy(io.Discard, r.Body)
				w.Write([]byte("saved"))
			}),
		},
	})

	client := &http.Client{
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}},
		Timeout:   2 * time.Second,
	}
	base := "https://127.0.0.1:" + itoa(port)

	var resp *http.Response
	for i := 0; i < 20; i++ {
		resp, err = client.Post(base+"/upload", "application/octet-stream", strings.NewReader("hi"))
		if err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("post /upload: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /upload over https = %d, want 200", resp.StatusCode)
	}
	if string(body) != "saved" {
		t.Errorf("body = %q, want %q", body, "saved")
	}

	// The page must still be served: mounting a route cannot shadow it.
	pr, err := client.Get(base + "/")
	if err != nil {
		t.Fatalf("get /: %v", err)
	}
	pr.Body.Close()
	if pr.StatusCode != http.StatusOK {
		t.Errorf("GET / = %d, want 200", pr.StatusCode)
	}
}
