package mode

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/saxill/beamdrop/internal/engine"
	"github.com/saxill/beamdrop/internal/spool"
	"github.com/saxill/beamdrop/internal/transfer"
)

// A JSON view of everything a front end needs, so the window can be written
// in whatever renders best rather than whatever binds to Go. The protocol,
// the pairing and the transfers stay here; the front end only draws and
// clicks.
//
// Loopback only, unlike /upload. A phone has a reason to reach the upload
// endpoint from across the tailnet; nothing has a reason to drive this
// machine's UI from another machine, so the door is narrower.

type apiOptions struct {
	Token    string
	InboxDir string
	Registry *engine.Registry
	Spool    *spool.Spool
	Pairs    *pairQueue
	Activity *activityLog
	Messages *messageLog
	Port     int
	Logf     func(string, ...any)
}

type apiState struct {
	Running  bool          `json:"running"`
	InboxDir string        `json:"inbox_dir"`
	URLs     []string      `json:"urls"`
	Peers    []apiPeer     `json:"peers"`
	Feed     []apiMessage  `json:"feed"`
	Pairing  []pendingPair `json:"pairing"`
	Spooled  int           `json:"spooled"`
	Activity []string      `json:"activity"`
}

type apiPeer struct {
	Name string `json:"name"`
	Key  string `json:"key"`
}

type apiMessage struct {
	At       int64  `json:"at"` // unix seconds, so a front end can format it
	Peer     string `json:"peer"`
	Outbound bool   `json:"outbound"`
	Kind     string `json:"kind"` // "text" | "file"
	Text     string `json:"text,omitempty"`
	FileName string `json:"file_name,omitempty"`
	Size     int64  `json:"size,omitempty"`
	Path     string `json:"path,omitempty"`
	IsImage  bool   `json:"is_image,omitempty"`
}

func apiHandler(opts apiOptions) http.Handler {
	mux := http.NewServeMux()

	guard := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			host, _, err := net.SplitHostPort(r.RemoteAddr)
			if err != nil {
				host = r.RemoteAddr
			}
			if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
				http.Error(w, "the API is local only", http.StatusForbidden)
				return
			}
			if !validToken(r, opts.Token) {
				http.Error(w, "bad or missing token", http.StatusUnauthorized)
				return
			}
			h(w, r)
		}
	}

	mux.HandleFunc("/api/state", guard(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, buildState(opts))
	}))

	mux.HandleFunc("/api/text", guard(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			To   string `json:"to"`
			Body string `json:"body"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		if strings.TrimSpace(req.Body) == "" {
			http.Error(w, "nothing to send", http.StatusBadRequest)
			return
		}
		n, err := forEachTarget(opts.Registry, req.To, func(e *engine.Engine) error {
			return e.SendText(req.Body)
		})
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		opts.Messages.record(Message{
			At: time.Now(), Peer: req.To, Outbound: true,
			Kind: MessageText, Text: req.Body,
		})
		opts.Logf("sent a message to %d device(s)", n)
		writeJSON(w, map[string]any{"sent": n})
	}))

	mux.HandleFunc("/api/file", guard(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			To   string `json:"to"`
			Path string `json:"path"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		if _, err := os.Stat(req.Path); err != nil {
			http.Error(w, "no such file", http.StatusBadRequest)
			return
		}
		n, err := forEachTarget(opts.Registry, req.To, func(e *engine.Engine) error {
			// One Sender per peer: it holds an open file handle and its own
			// read offset, so it cannot be shared across transfers.
			s, err := transfer.NewSender(req.Path)
			if err != nil {
				return err
			}
			defer s.Close()
			return e.SendFile(s, nil)
		})
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		info, _ := os.Stat(req.Path)
		var size int64
		if info != nil {
			size = info.Size()
		}
		opts.Messages.record(Message{
			At: time.Now(), Peer: req.To, Outbound: true, Kind: MessageFile,
			FileName: filepath.Base(req.Path), Size: size, Path: req.Path,
		})
		writeJSON(w, map[string]any{"sent": n})
	}))

	mux.HandleFunc("/api/pairing", guard(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, opts.Pairs.List())
			return
		}
		var req struct {
			ID     string `json:"id"`
			Accept bool   `json:"accept"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		if !opts.Pairs.Respond(req.ID, req.Accept) {
			// Stale click from a window that had not refreshed. Saying so
			// beats pretending it worked.
			http.Error(w, "that request is no longer waiting", http.StatusConflict)
			return
		}
		writeJSON(w, map[string]any{"ok": true})
	}))

	return mux
}

func buildState(opts apiOptions) apiState {
	// Empty slices, never nil: a nil slice marshals to JSON null, and a
	// front end looping over it crashes rather than drawing nothing.
	st := apiState{
		Running:  true,
		InboxDir: opts.InboxDir,
		URLs:     reachableURLs(opts.Port),
		Pairing:  opts.Pairs.List(),
		Peers:    []apiPeer{},
		Feed:     []apiMessage{},
		Activity: []string{},
	}
	for _, e := range opts.Registry.All() {
		k := e.PeerPubKey()
		st.Peers = append(st.Peers, apiPeer{Name: e.PeerName(), Key: fmt.Sprintf("%x", k[:4])})
	}
	sort.Slice(st.Peers, func(i, j int) bool { return st.Peers[i].Name < st.Peers[j].Name })

	for _, m := range opts.Messages.feed(opts.InboxDir, 300) {
		am := apiMessage{
			At: m.At.Unix(), Peer: m.Peer, Outbound: m.Outbound,
			Text: m.Text, FileName: m.FileName, Size: m.Size, Path: m.Path,
		}
		if m.Kind == MessageFile {
			am.Kind = "file"
			am.IsImage = isImageName(m.FileName)
		} else {
			am.Kind = "text"
		}
		st.Feed = append(st.Feed, am)
	}
	if opts.Spool != nil {
		if items, err := opts.Spool.Pending(); err == nil {
			st.Spooled = len(items)
		}
	}
	if opts.Activity != nil {
		for _, e := range opts.Activity.Recent(40) {
			st.Activity = append(st.Activity, e.At.Format("15:04:05")+"  "+e.Text)
		}
	}
	return st
}

// forEachTarget runs fn against one named peer, or every connected peer
// when the name is empty.
func forEachTarget(reg *engine.Registry, to string, fn func(*engine.Engine) error) (int, error) {
	peers := reg.All()
	if len(peers) == 0 {
		return 0, fmt.Errorf("nobody is connected to send to")
	}
	var (
		sent int
		last error
	)
	for _, e := range peers {
		if to != "" && e.PeerName() != to {
			continue
		}
		if err := fn(e); err != nil {
			last = fmt.Errorf("%s: %w", e.PeerName(), err)
			continue
		}
		sent++
	}
	if sent == 0 {
		if last != nil {
			return 0, last
		}
		return 0, fmt.Errorf("%q is not connected", to)
	}
	return sent, nil
}

func isImageName(name string) bool {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".jpg", ".jpeg", ".png", ".gif", ".webp", ".bmp":
		return true
	}
	return false
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(v)
}
