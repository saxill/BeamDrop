package mode

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/saxill/beamdrop/internal/pairing"
	"github.com/saxill/beamdrop/internal/peer"
)

const testToken = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func postUpload(t *testing.T, h http.Handler, body, name, token, remote string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/upload", strings.NewReader(body))
	if name != "" {
		r.Header.Set("X-Beamdrop-Filename", name)
	}
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	r.RemoteAddr = remote
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestUploadWritesToInbox(t *testing.T) {
	inbox := t.TempDir()
	h := uploadHandler(inbox, testToken, func(string, ...any) {})

	w := postUpload(t, h, "hello from shortcuts", "note.txt", testToken, "100.64.0.2:5000")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body)
	}
	got, err := os.ReadFile(filepath.Join(inbox, "note.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "hello from shortcuts" {
		t.Errorf("body = %q", got)
	}
}

// TestUploadRequiresBothTailnetAndToken is the whole security argument for
// this endpoint: it is guarded by a bearer token, which is weaker than the
// X25519 pairing the frame protocol uses, so a leaked token must not be
// sufficient on its own.
func TestUploadRequiresBothTailnetAndToken(t *testing.T) {
	inbox := t.TempDir()
	h := uploadHandler(inbox, testToken, func(string, ...any) {})

	for _, tc := range []struct {
		name   string
		token  string
		remote string
		want   int
	}{
		{"right token, off-tailnet address", testToken, "203.0.113.9:5000", http.StatusForbidden},
		{"right token, private LAN address", testToken, "192.168.1.50:5000", http.StatusForbidden},
		{"on tailnet, no token", "", "100.64.0.2:5000", http.StatusUnauthorized},
		{"on tailnet, wrong token", "deadbeef", "100.64.0.2:5000", http.StatusUnauthorized},
		{"both correct", testToken, "100.64.0.2:5000", http.StatusOK},
		{"loopback counts as trusted", testToken, "127.0.0.1:5000", http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := postUpload(t, h, "x", "f-"+strings.ReplaceAll(tc.name, " ", "_"), tc.token, tc.remote)
			if w.Code != tc.want {
				t.Errorf("status = %d, want %d (%s)", w.Code, tc.want, w.Body)
			}
		})
	}
}

// TestUploadNameIsConfinedToTheInbox: the filename is entirely
// client-controlled, exactly like FILE_OFFER.Name on the frame path, and
// filepath.Join cleans "../" rather than rejecting it.
func TestUploadNameIsConfinedToTheInbox(t *testing.T) {
	root := t.TempDir()
	inbox := filepath.Join(root, "inbox")
	if err := os.MkdirAll(inbox, 0o755); err != nil {
		t.Fatal(err)
	}
	h := uploadHandler(inbox, testToken, func(string, ...any) {})

	for _, name := range []string{"../escaped.bin", "../../escaped.bin", "/etc/escaped.bin", "a/b/escaped.bin"} {
		w := postUpload(t, h, "pwned", name, testToken, "100.64.0.2:5000")
		if w.Code != http.StatusOK {
			t.Fatalf("%s: status %d", name, w.Code)
		}
		if _, err := os.Stat(filepath.Join(root, "escaped.bin")); err == nil {
			t.Fatalf("%q escaped the inbox", name)
		}
		if _, err := os.Stat(filepath.Join(inbox, "escaped.bin")); err != nil {
			t.Errorf("%q did not land in the inbox: %v", name, err)
		}
		os.Remove(filepath.Join(inbox, "escaped.bin"))
	}
}

func TestUploadTokenIsStableAndPrivate(t *testing.T) {
	dir := t.TempDir()
	first, err := loadOrCreateUploadToken(dir)
	if err != nil {
		t.Fatal(err)
	}
	second, err := loadOrCreateUploadToken(dir)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Error("token changed between calls — every shortcut would break on restart")
	}
	if len(first) != 64 {
		t.Errorf("token is %d chars, want 64 hex chars", len(first))
	}
	info, err := os.Stat(filepath.Join(dir, uploadTokenFile))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("token file is mode %#o, want no group/other access", perm)
	}
}

func TestUploadRejectsGet(t *testing.T) {
	h := uploadHandler(t.TempDir(), testToken, func(string, ...any) {})
	r := httptest.NewRequest(http.MethodGet, "/upload?token="+testToken, nil)
	r.RemoteAddr = "100.64.0.2:5000"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET /upload = %d, want 405", w.Code)
	}
}

// TestUploadOverPlainHTTPThroughTheRealServer proves the wiring: Shortcuts
// will send plain HTTP to this port, and it has to reach the handler rather
// than the https redirect.
func TestUploadOverPlainHTTPThroughTheRealServer(t *testing.T) {
	inbox := t.TempDir()
	known, err := pairing.NewKnownPeers(t.TempDir(), peer.NewMemStore())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv, err := startPeerServer(ctx, peerServerOptions{
		ListenAddr:  "127.0.0.1:0",
		InboxDir:    inbox,
		Known:       known,
		Confirmer:   func(a, b [32]byte, name, code string, known bool) bool { return true },
		UploadToken: testToken,
		Logf:        func(string, ...any) {},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	body := bytes.Repeat([]byte("photo"), 5000)
	req, err := http.NewRequest(http.MethodPost, "http://"+srv.Addr().String()+"/upload", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+testToken)
	req.Header.Set("X-Beamdrop-Filename", "IMG_0001.jpeg")

	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("plain http upload: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	got, err := os.ReadFile(filepath.Join(inbox, "IMG_0001.jpeg"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, body) {
		t.Errorf("received %d bytes, want %d", len(got), len(body))
	}

	// Everything else on the plain port still redirects to https.
	noRedirect := &http.Client{
		Timeout:       5 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	r2, err := noRedirect.Get("http://" + srv.Addr().String() + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer r2.Body.Close()
	if r2.StatusCode != http.StatusPermanentRedirect {
		t.Errorf("GET / = %d, want the https redirect", r2.StatusCode)
	}
}

func TestIsTailnetAddr(t *testing.T) {
	for _, tc := range []struct {
		addr string
		want bool
	}{
		{"100.64.0.2:1", true},   // tailnet v4, low end of 100.64/10
		{"100.127.255.255:1", true}, // top of the range
		{"[fd7a:115c:a1e0::cf37:f133]:1", true},
		{"127.0.0.1:1", true},
		{"[::1]:1", true},
		{"100.63.0.1:1", false},  // just below the CGNAT range
		{"100.128.0.1:1", false}, // just above it
		{"192.168.1.5:1", false},
		{"8.8.8.8:1", false},
		{"not-an-address", false},
	} {
		if got := isTailnetAddr(tc.addr); got != tc.want {
			t.Errorf("isTailnetAddr(%q) = %v, want %v", tc.addr, got, tc.want)
		}
	}
	_ = net.IPv4len
}

// TestUploadRecoversMissingExtension: iOS Shortcuts' "Name" variable is the
// base name without the extension, so a shared photo arrived as "IMG_6302"
// and would not open on the laptop.
func TestUploadRecoversMissingExtension(t *testing.T) {
	inbox := t.TempDir()
	h := uploadHandler(inbox, testToken, func(string, ...any) {})

	for _, tc := range []struct{ sent, contentType, want string }{
		{"IMG_6302", "image/jpeg", "IMG_6302.jpeg"},
		{"clip", "video/quicktime", "clip.mov"},
		{"doc", "application/pdf", "doc.pdf"},
		{"already.png", "image/jpeg", "already.png"},       // never override a real one
		{"unknown", "application/octet-stream", "unknown"}, // nothing to infer
		{"nocontenttype", "", "nocontenttype"},
	} {
		r := httptest.NewRequest(http.MethodPost, "/upload", strings.NewReader("x"))
		r.Header.Set("X-Beamdrop-Filename", tc.sent)
		r.Header.Set("Authorization", "Bearer "+testToken)
		if tc.contentType != "" {
			r.Header.Set("Content-Type", tc.contentType)
		}
		r.RemoteAddr = "100.64.0.2:5000"
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("%s: status %d", tc.sent, w.Code)
		}
		if _, err := os.Stat(filepath.Join(inbox, tc.want)); err != nil {
			got, _ := os.ReadDir(inbox)
			names := []string{}
			for _, e := range got {
				names = append(names, e.Name())
			}
			t.Errorf("sent %q as %q, wanted %q on disk; inbox has %v", tc.sent, tc.contentType, tc.want, names)
		}
	}
}
