package mode

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/saxill/beamdrop/internal/engine"
	"github.com/saxill/beamdrop/internal/frame"
	"github.com/saxill/beamdrop/internal/pairing"
	"github.com/saxill/beamdrop/internal/peer"
)

func apiServer(t *testing.T) (*peerServer, string) {
	t.Helper()
	known, err := pairing.NewKnownPeers(t.TempDir(), peer.NewMemStore())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	srv, err := startPeerServer(ctx, peerServerOptions{
		ListenAddr:  "127.0.0.1:0",
		InboxDir:    t.TempDir(),
		Known:       known,
		UploadToken: testToken,
		Logf:        func(string, ...any) {},
		// No Confirmer: the server's own pairing queue takes over, which is
		// the arrangement an API-driven front end relies on.
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Close() })
	return srv, "http://" + srv.Addr().String()
}

func apiGet(t *testing.T, base, path string) (*http.Response, []byte) {
	t.Helper()
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Get(base + path + "?token=" + testToken)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b := make([]byte, 1<<20)
	n, _ := resp.Body.Read(b)
	return resp, b[:n]
}

func apiPost(t *testing.T, base, path string, body any) *http.Response {
	t.Helper()
	raw, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPost, base+path+"?token="+testToken, bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestAPIStateReportsInboxAndPeers(t *testing.T) {
	srv, base := apiServer(t)
	if err := os.WriteFile(filepath.Join(srv.opts.InboxDir, "photo.jpeg"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	resp, body := apiGet(t, base, "/api/state")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.StatusCode, body)
	}
	var st apiState
	if err := json.Unmarshal(body, &st); err != nil {
		t.Fatalf("bad json: %v — %s", err, body)
	}
	if len(st.Feed) != 1 || st.Feed[0].FileName != "photo.jpeg" {
		t.Fatalf("feed = %+v, want the inbox file", st.Feed)
	}
	if !st.Feed[0].IsImage {
		t.Error("a .jpeg was not flagged as an image, so no front end will preview it")
	}
	if st.InboxDir == "" || len(st.URLs) == 0 {
		t.Error("state is missing the inbox path or the URLs to show the user")
	}
}

// TestAPIIsLoopbackOnly: /upload has a reason to be reachable across the
// tailnet; nothing has a reason to drive this machine's UI from another
// machine.
func TestAPIIsLoopbackOnly(t *testing.T) {
	opts := apiOptions{
		Token: testToken, Pairs: newPairQueue(), Messages: newMessageLog(),
		Registry: engine.NewRegistry(), Logf: func(string, ...any) {},
	}
	h := apiHandler(opts)

	for _, tc := range []struct {
		remote string
		want   int
	}{
		{"127.0.0.1:5000", http.StatusOK},
		{"[::1]:5000", http.StatusOK},
		{"100.64.0.2:5000", http.StatusForbidden}, // on the tailnet, still refused
		{"192.168.1.9:5000", http.StatusForbidden},
	} {
		r, _ := http.NewRequest(http.MethodGet, "/api/state?token="+testToken, nil)
		r.RemoteAddr = tc.remote
		w := newRecorder()
		h.ServeHTTP(w, r)
		if w.code != tc.want {
			t.Errorf("from %s = %d, want %d", tc.remote, w.code, tc.want)
		}
	}
}

func TestAPIRequiresToken(t *testing.T) {
	_, base := apiServer(t)
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Get(base + "/api/state")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("no token = %d, want 401", resp.StatusCode)
	}
}

func TestAPISendWithNobodyConnected(t *testing.T) {
	_, base := apiServer(t)
	resp := apiPost(t, base, "/api/text", map[string]any{"body": "hello"})
	defer resp.Body.Close()
	// A front end must be able to tell "nothing happened" from "it worked".
	if resp.StatusCode == http.StatusOK {
		t.Error("sending with no peers reported success")
	}
}

// TestAPIPairingCanBeAnsweredOverHTTP is the whole reason the queue exists:
// the prompt used to be answerable only by the process that owned the
// terminal.
func TestAPIPairingCanBeAnsweredOverHTTP(t *testing.T) {
	srv, base := apiServer(t)

	paired := make(chan error, 1)
	go func() {
		conn, err := net.DialTimeout("tcp", srv.Addr().String(), 5*time.Second)
		if err != nil {
			paired <- err
			return
		}
		defer conn.Close()
		_, err = engine.New(engine.Config{
			Name: "iPhone", Conn: conn, IsInitiator: true,
			Confirmer: func(a, b [32]byte, n, c string, k bool) bool { return true },
		})
		paired <- err
		time.Sleep(time.Second)
	}()

	// Wait for the request to surface in the API.
	var pending []pendingPair
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		_, body := apiGet(t, base, "/api/pairing")
		if err := json.Unmarshal(body, &pending); err == nil && len(pending) == 1 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if len(pending) != 1 {
		t.Fatal("the pairing request never appeared in the API")
	}
	if pending[0].Peer != "iPhone" || len(pending[0].Code) != 6 {
		t.Errorf("pending = %+v", pending[0])
	}

	// Still blocked until answered.
	select {
	case err := <-paired:
		t.Fatalf("pairing completed before anything answered (err=%v)", err)
	case <-time.After(300 * time.Millisecond):
	}

	resp := apiPost(t, base, "/api/pairing", map[string]any{"id": pending[0].ID, "accept": true})
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("accept = %d", resp.StatusCode)
	}
	select {
	case err := <-paired:
		if err != nil {
			t.Errorf("pairing failed after accept: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("pairing did not complete after being accepted over the API")
	}

	// Answering it twice must not look like it worked.
	resp2 := apiPost(t, base, "/api/pairing", map[string]any{"id": pending[0].ID, "accept": true})
	resp2.Body.Close()
	if resp2.StatusCode == http.StatusOK {
		t.Error("a stale pairing id was accepted a second time")
	}
}

func TestAPITextReachesAConnectedPeer(t *testing.T) {
	srv, base := apiServer(t)

	got := make(chan string, 2)
	conn, err := net.DialTimeout("tcp", srv.Addr().String(), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	done := make(chan error, 1)
	go func() {
		e, err := engine.New(engine.Config{
			Name: "iPhone", Conn: conn, IsInitiator: true,
			Confirmer: func(a, b [32]byte, n, c string, k bool) bool { return true },
		})
		if err != nil {
			done <- err
			return
		}
		e.OnText(func(body string) { got <- body })
		e.OnFileOffer(func(frame.FileOfferPayload) (string, bool) { return t.TempDir(), true })
		go e.Serve()
		done <- nil
	}()

	// Accept the pairing the queue is holding.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var pending []pendingPair
		_, body := apiGet(t, base, "/api/pairing")
		if err := json.Unmarshal(body, &pending); err == nil && len(pending) == 1 {
			apiPost(t, base, "/api/pairing", map[string]any{"id": pending[0].ID, "accept": true}).Body.Close()
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err := <-done; err != nil {
		t.Fatalf("pair: %v", err)
	}

	resp := apiPost(t, base, "/api/text", map[string]any{"body": "sent from the API"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("send = %d", resp.StatusCode)
	}
	select {
	case body := <-got:
		if body != "sent from the API" {
			t.Errorf("peer received %q", body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the message never reached the peer")
	}

	// And it shows up in the feed the front end draws.
	_, body := apiGet(t, base, "/api/state")
	if !strings.Contains(string(body), "sent from the API") {
		t.Error("the sent message is missing from the feed")
	}
}

// newRecorder is a tiny ResponseWriter, to keep this file free of a
// dependency on httptest's larger surface for two assertions.
type recorder struct {
	code int
	hdr  http.Header
	body bytes.Buffer
}

func newRecorder() *recorder { return &recorder{code: 200, hdr: http.Header{}} }

func (r *recorder) Header() http.Header         { return r.hdr }
func (r *recorder) Write(b []byte) (int, error) { return r.body.Write(b) }
func (r *recorder) WriteHeader(c int)           { r.code = c }

// TestAPINeverEmitsNullCollections: a nil Go slice marshals to JSON null,
// and a front end looping over null crashes instead of drawing nothing.
func TestAPINeverEmitsNullCollections(t *testing.T) {
	_, base := apiServer(t)
	_, body := apiGet(t, base, "/api/state")
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"peers", "feed", "pairing", "activity", "urls"} {
		v, ok := raw[key]
		if !ok {
			t.Errorf("state is missing %q entirely", key)
			continue
		}
		if v == nil {
			t.Errorf("%q is null; it must be an empty array", key)
		}
	}
}
