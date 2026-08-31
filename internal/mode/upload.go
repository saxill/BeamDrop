package mode

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/saxill/beamdrop/internal/spool"
)

// The upload endpoint exists for one reason: iOS Shortcuts. Its "Get
// Contents of URL" action speaks plain HTTP and nothing else — it cannot
// run the pairing ceremony or the frame protocol, so the share sheet is
// unreachable without a door it can actually open.
//
// That door is deliberately narrower than the front one. The frame protocol
// authenticates a peer with X25519 and a confirmed code; this authenticates
// a request with a bearer token, which is weaker. Two things make up for it:
//
//   - It is reachable only from the tailnet (or this machine). A leaked
//     token is not enough on its own — an attacker also has to be a node on
//     your tailnet, which is a much higher bar than knowing a string.
//   - It is served over plain HTTP on purpose. Tailscale traffic is already
//     WireGuard-encrypted end to end, so TLS here would be double encryption
//     for no gain — and Shortcuts refuses a self-signed certificate outright,
//     with no tap-through, which would make HTTPS the thing that breaks it.

// uploadTokenFile is where the shared secret lives, inside the config dir.
const uploadTokenFile = "upload.token"

// maxUpload caps a single request body. Generous for photos and short
// video, bounded so a stuck or hostile client cannot fill the disk.
const maxUpload = 2 << 30 // 2 GiB

// loadOrCreateUploadToken returns the bearer token, minting one on first
// use. It is 32 random bytes hex-encoded — long enough that guessing is not
// a concern even without rate limiting.
func loadOrCreateUploadToken(dir string) (string, error) {
	path := filepath.Join(dir, uploadTokenFile)
	if b, err := os.ReadFile(path); err == nil {
		if tok := strings.TrimSpace(string(b)); tok != "" {
			return tok, nil
		}
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("upload token: %w", err)
	}

	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	tok := hex.EncodeToString(raw[:])
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	if err := os.WriteFile(path, []byte(tok+"\n"), 0o600); err != nil {
		return "", fmt.Errorf("upload token: %w", err)
	}
	return tok, nil
}

// isTailnetAddr reports whether a request came from somewhere allowed to
// upload: a Tailscale node, or this machine itself.
//
// Tailscale hands out 100.64.0.0/10 (the CGNAT range) and fd7a:115c:a1e0::/48.
// Loopback is allowed because a local process could write to the inbox
// directly anyway — refusing it would buy nothing.
func isTailnetAddr(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	if ip.IsLoopback() {
		return true
	}
	if v4 := ip.To4(); v4 != nil {
		return v4[0] == 100 && v4[1] >= 64 && v4[1] <= 127
	}
	// fd7a:115c:a1e0::/48
	return len(ip) == net.IPv6len &&
		ip[0] == 0xfd && ip[1] == 0x7a && ip[2] == 0x11 && ip[3] == 0x5c &&
		ip[4] == 0xa1 && ip[5] == 0xe0
}

// uploadedName picks the filename to write, from the header Shortcuts can
// set or a query parameter, defaulting to something obviously generated.
//
// filepath.Base is the same sanitiser the frame path uses: the name is
// entirely client-controlled, and filepath.Join *cleans* "../" rather than
// rejecting it, so anything but the bare base name is an escape waiting to
// happen.
func uploadedName(r *http.Request) string {
	name := r.Header.Get("X-Beamdrop-Filename")
	if name == "" {
		name = r.URL.Query().Get("name")
	}
	name = filepath.Base(strings.TrimSpace(name))
	if name == "" || name == "." || name == string(filepath.Separator) {
		name = "upload"
	}
	// iOS Shortcuts' "Name" variable is the base name *without* the
	// extension, so a photo shared from the share sheet arrives as
	// "IMG_6302" and the laptop has no idea it is a JPEG. Recover it from
	// the Content-Type the client sent.
	if filepath.Ext(name) == "" {
		if ext := extensionFor(r.Header.Get("Content-Type")); ext != "" {
			name += ext
		}
	}
	return name
}

// extensionFor maps a content type to a file extension, preferring a short
// canonical one over whatever mime.ExtensionsByType happens to sort first
// (it offers ".jpe" for image/jpeg).
func extensionFor(contentType string) string {
	mt, _, err := mime.ParseMediaType(contentType)
	if err != nil || mt == "" || mt == "application/octet-stream" {
		return ""
	}
	if ext, ok := preferredExt[mt]; ok {
		return ext
	}
	exts, err := mime.ExtensionsByType(mt)
	if err != nil || len(exts) == 0 {
		return ""
	}
	shortest := exts[0]
	for _, e := range exts[1:] {
		if len(e) < len(shortest) {
			shortest = e
		}
	}
	return shortest
}

var preferredExt = map[string]string{
	"image/jpeg":      ".jpeg",
	"image/png":       ".png",
	"image/heic":      ".heic",
	"image/gif":       ".gif",
	"image/webp":      ".webp",
	"video/mp4":       ".mp4",
	"video/quicktime": ".mov",
	"audio/mpeg":      ".mp3",
	"application/pdf": ".pdf",
	"text/plain":      ".txt",
}

// uploadRoute decides where an accepted upload goes.
type uploadRoute struct {
	InboxDir string
	Token    string
	// SelfName is this node's own name. An upload addressed to it (or to
	// nobody) lands in the local inbox.
	SelfName string
	// Spool, when set, lets this node accept files for a peer that is not
	// reachable right now and deliver them later. Without it, an upload
	// addressed elsewhere is refused rather than silently kept.
	Spool *spool.Spool
	// KnownNames is who this node can forward to. An upload for a name not
	// in here is rejected at the door: spooling it would mean holding a
	// file forever for a peer that will never be resolvable.
	KnownNames func() []string
	// DefaultTo is where an upload with no destination header goes. A relay
	// exists to pass things on, so "no header" meaning "keep it here" made
	// the common case the one that needed extra configuration — and on iOS
	// every header is another row to type on a phone keyboard. Empty
	// restores the old behaviour of keeping it.
	DefaultTo string
	Logf      func(string, ...any)
}

// uploadHandler accepts a file body and either writes it into the inbox or
// spools it for another peer.
func uploadHandlerRouted(rt uploadRoute) http.Handler {
	logf := rt.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A refused upload used to return and say nothing, so a phone that
		// was quietly being turned away left the log looking exactly like a
		// phone that had never sent anything at all.
		if r.Method != http.MethodPost && r.Method != http.MethodPut {
			w.Header().Set("Allow", "POST, PUT")
			logf("refused %s /upload from %s: use POST", r.Method, r.RemoteAddr)
			http.Error(w, "use POST", http.StatusMethodNotAllowed)
			return
		}
		if !isTailnetAddr(r.RemoteAddr) {
			logf("refused upload from %s: not a tailnet address", r.RemoteAddr)
			http.Error(w, "uploads are only accepted from the tailnet", http.StatusForbidden)
			return
		}
		if !validToken(r, rt.Token) {
			logf("refused upload from %s: bad or missing token", r.RemoteAddr)
			http.Error(w, "bad or missing token", http.StatusUnauthorized)
			return
		}

		name := uploadedName(r)
		to := strings.TrimSpace(r.Header.Get("X-Beamdrop-To"))
		if to == "" {
			to = strings.TrimSpace(r.URL.Query().Get("to"))
		}
		body := http.MaxBytesReader(w, r.Body, maxUpload)

		if to == "" {
			to = rt.DefaultTo
		}
		// Addressed here, or addressed nowhere with no default: keep it.
		if to == "" || strings.EqualFold(to, rt.SelfName) || strings.EqualFold(to, "here") {
			writeToInbox(w, body, rt.InboxDir, name, logf)
			return
		}

		if rt.Spool == nil {
			http.Error(w, "this node does not relay; omit X-Beamdrop-To to keep the file here", http.StatusBadRequest)
			return
		}
		if !knownName(rt.KnownNames, to) {
			// Refusing now beats holding a file forever for a peer that can
			// never be resolved.
			http.Error(w, fmt.Sprintf("unknown destination %q; pair it with this relay first", to), http.StatusBadRequest)
			return
		}
		item, err := rt.Spool.Add(to, name, body)
		if err != nil {
			logf("spool %s for %s failed: %v", name, to, err)
			http.Error(w, "could not spool the file", http.StatusInternalServerError)
			return
		}
		logf("held %s (%d bytes) for %s", item.Name, item.Size, to)
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintf(w, "held %s (%d bytes) for %s\n", item.Name, item.Size, to)
	})
}

func knownName(names func() []string, want string) bool {
	if names == nil {
		return false
	}
	for _, n := range names() {
		if strings.EqualFold(n, want) {
			return true
		}
	}
	return false
}

func writeToInbox(w http.ResponseWriter, body io.Reader, inboxDir, name string, logf func(string, ...any)) {
	if err := os.MkdirAll(inboxDir, 0o755); err != nil {
		http.Error(w, "inbox unavailable", http.StatusInternalServerError)
		return
	}
	dest := filepath.Join(inboxDir, name)
	f, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		http.Error(w, "cannot write to the inbox", http.StatusInternalServerError)
		return
	}
	n, copyErr := io.Copy(f, body)
	closeErr := f.Close()
	if copyErr != nil || closeErr != nil {
		_ = os.Remove(dest)
		logf("upload %s failed after %d bytes: %v", name, n, cmpErr(copyErr, closeErr))
		http.Error(w, "upload failed", http.StatusInternalServerError)
		return
	}
	logf("received %s (%d bytes) via shortcut", name, n)
	notifyArrival(name, n)
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	fmt.Fprintf(w, "saved %s (%d bytes)\n", name, n)
}

// uploadHandler is the inbox-only form, kept for callers that do not relay.
func uploadHandler(inboxDir, token string, logf func(string, ...any)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost && r.Method != http.MethodPut {
			w.Header().Set("Allow", "POST, PUT")
			http.Error(w, "use POST", http.StatusMethodNotAllowed)
			return
		}
		// Network position first: a caller that is not on the tailnet learns
		// nothing about whether its token would have been right.
		if !isTailnetAddr(r.RemoteAddr) {
			http.Error(w, "uploads are only accepted from the tailnet", http.StatusForbidden)
			return
		}
		if !validToken(r, token) {
			http.Error(w, "bad or missing token", http.StatusUnauthorized)
			return
		}

		name := uploadedName(r)
		if err := os.MkdirAll(inboxDir, 0o755); err != nil {
			http.Error(w, "inbox unavailable", http.StatusInternalServerError)
			return
		}
		dest := filepath.Join(inboxDir, name)
		f, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
		if err != nil {
			http.Error(w, "cannot write to the inbox", http.StatusInternalServerError)
			return
		}
		n, copyErr := io.Copy(f, http.MaxBytesReader(w, r.Body, maxUpload))
		closeErr := f.Close()
		if copyErr != nil || closeErr != nil {
			// A partial file is worse than none — it looks like a successful
			// transfer to anyone browsing the inbox later.
			_ = os.Remove(dest)
			logf("upload %s failed after %d bytes: %v", name, n, cmpErr(copyErr, closeErr))
			http.Error(w, "upload failed", http.StatusInternalServerError)
			return
		}
		logf("received %s (%d bytes) via shortcut", name, n)
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintf(w, "saved %s (%d bytes)\n", name, n)
	})
}

func cmpErr(a, b error) error {
	if a != nil {
		return a
	}
	return b
}

// validToken accepts the token as a bearer header or as ?token=, since
// Shortcuts makes headers slightly fiddlier than query parameters.
// Comparison is constant time.
func validToken(r *http.Request, want string) bool {
	got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if got == "" {
		got = r.Header.Get("X-Beamdrop-Token")
	}
	if got == "" {
		got = r.URL.Query().Get("token")
	}
	return subtle.ConstantTimeCompare([]byte(strings.TrimSpace(got)), []byte(want)) == 1
}
