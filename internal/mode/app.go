package mode

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/saxill/beamdrop/internal/engine"
	"github.com/saxill/beamdrop/internal/pairing"
	"github.com/saxill/beamdrop/internal/peer"
	"github.com/saxill/beamdrop/internal/spool"
	"github.com/saxill/beamdrop/internal/transfer"
)

// App is the desktop front end's view of beamdrop, with no toolkit in it.
//
// Keeping the widget library out of here is the point: a GUI is the part of
// a program most likely to be rewritten or thrown away, and the least
// pleasant to test. Everything a window needs to draw — who is connected,
// what arrived, what is waiting — is answerable without one, so it is
// answered here and the window only renders.
//
// It runs the portal in-process. A desktop app that required you to first
// start a server in a terminal would not have solved anything.
type App struct {
	opts PortalOptions

	mu       sync.Mutex
	srv      *peerServer
	cancel   context.CancelFunc
	spool    *spool.Spool
	activity *activityLog
	started  time.Time

	messages []Message

	pairCh   chan PairRequest
	onChange func()
}

// PairRequest is a peer asking to be trusted. The window shows the code and
// calls Accept or Reject; nothing happens until it does.
type PairRequest struct {
	PeerName string
	Code     string
	respond  chan bool
}

func (p PairRequest) Accept() { p.respond <- true }
func (p PairRequest) Reject() { p.respond <- false }

// Status is everything a window needs for its header.
type Status struct {
	Running   bool
	Since     time.Time
	InboxDir  string
	URLs      []string
	Peers     []string
	Spooled   int
	Relay     bool
	LastError string
}

// InboxFile is one received file.
type InboxFile struct {
	Name     string
	Path     string
	Size     int64
	Received time.Time
}

func NewApp(opts PortalOptions) *App {
	return &App{opts: opts, pairCh: make(chan PairRequest, 4)}
}

// OnChange registers a callback fired whenever something happened that a
// window would want to redraw for. It is called from background goroutines,
// so a toolkit that demands its own thread has to marshal.
func (a *App) OnChange(fn func()) {
	a.mu.Lock()
	a.onChange = fn
	a.mu.Unlock()
}

func (a *App) changed() {
	a.mu.Lock()
	fn := a.onChange
	a.mu.Unlock()
	if fn != nil {
		fn()
	}
}

// PairRequests yields peers waiting to be confirmed.
func (a *App) PairRequests() <-chan PairRequest { return a.pairCh }

// Start brings the portal up. Safe to call when already running.
func (a *App) Start() error {
	a.mu.Lock()
	if a.srv != nil {
		a.mu.Unlock()
		return nil
	}
	a.mu.Unlock()

	known, err := pairing.NewKnownPeers(a.opts.KnownPeersDir, peer.NewMemStore())
	if err != nil {
		return err
	}
	identity, err := pairing.LoadOrCreateIdentity(a.opts.ConfigDir)
	if err != nil {
		return err
	}
	activity := newActivityLog(nil)

	var sp *spool.Spool
	if a.opts.Relay {
		if sp, err = spool.Open(filepath.Join(a.opts.ConfigDir, "spool")); err != nil {
			return err
		}
	}
	token, _ := loadOrCreateUploadToken(a.opts.ConfigDir)

	logf := func(format string, args ...any) {
		activity.Logf(format, args...)
		a.changed()
	}

	ctx, cancel := context.WithCancel(context.Background())
	srv, err := startPeerServer(ctx, peerServerOptions{
		ListenAddr:  fmt.Sprintf(":%d", a.opts.Port),
		InboxDir:    a.opts.InboxDir,
		ConfigDir:   a.opts.ConfigDir,
		Known:       known,
		Identity:    &identity,
		Spool:       sp,
		SpoolMaxAge: a.opts.SpoolMaxAge,
		UploadToken: token,
		Confirmer: func(_, _ [32]byte, peerName, code string, isKnown bool) bool {
			if isKnown {
				return true
			}
			req := PairRequest{PeerName: peerName, Code: code, respond: make(chan bool, 1)}
			select {
			case a.pairCh <- req:
			case <-ctx.Done():
				return false
			}
			a.changed()
			// Blocks until the window answers, which is what makes the
			// prompt meaningful rather than decorative.
			select {
			case ok := <-req.respond:
				return ok
			case <-ctx.Done():
				return false
			}
		},
		OnEngine: func(e *engine.Engine) {
			name := e.PeerName()
			e.OnText(func(body string) {
				a.record(Message{At: time.Now(), Peer: name, Kind: MessageText, Text: body})
				logf("%s: %s", name, firstLine(body))
			})
		},
		Logf: logf,
	})
	if err != nil {
		cancel()
		return err
	}

	a.mu.Lock()
	a.srv, a.cancel, a.spool, a.activity, a.started = srv, cancel, sp, activity, time.Now()
	a.mu.Unlock()
	logf("beamdrop started on port %d", a.opts.Port)
	a.changed()
	return nil
}

// Stop shuts the portal down. Safe to call when not running.
func (a *App) Stop() {
	a.mu.Lock()
	srv, cancel := a.srv, a.cancel
	a.srv, a.cancel = nil, nil
	a.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if srv != nil {
		srv.Close()
	}
	a.changed()
}

func (a *App) Status() Status {
	a.mu.Lock()
	srv, sp, started := a.srv, a.spool, a.started
	a.mu.Unlock()

	st := Status{
		InboxDir: a.opts.InboxDir,
		Relay:    a.opts.Relay,
		Running:  srv != nil,
		Since:    started,
	}
	if srv == nil {
		return st
	}
	st.URLs = reachableURLs(a.opts.Port)
	for _, e := range srv.Registry().All() {
		st.Peers = append(st.Peers, e.PeerName())
	}
	sort.Strings(st.Peers)
	if sp != nil {
		if items, err := sp.Pending(); err == nil {
			st.Spooled = len(items)
		}
	}
	return st
}

// Inbox lists received files, newest first.
func (a *App) Inbox(limit int) []InboxFile {
	entries, err := os.ReadDir(a.opts.InboxDir)
	if err != nil {
		return nil
	}
	var out []InboxFile
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, InboxFile{
			Name:     e.Name(),
			Path:     filepath.Join(a.opts.InboxDir, e.Name()),
			Size:     info.Size(),
			Received: info.ModTime(),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Received.After(out[j].Received) })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// Activity returns recent log lines, newest first.
func (a *App) Activity(n int) []string {
	a.mu.Lock()
	act := a.activity
	a.mu.Unlock()
	if act == nil {
		return nil
	}
	var out []string
	for _, e := range act.Recent(n) {
		out = append(out, e.At.Format("15:04:05")+"  "+e.Text)
	}
	return out
}

// Send pushes a file to every connected peer, reporting how many got it.
// Dropping a file on a window with nobody connected is a mistake worth
// naming rather than a silent no-op.
func (a *App) Send(path string) (int, error) {
	a.mu.Lock()
	srv := a.srv
	a.mu.Unlock()
	if srv == nil {
		return 0, fmt.Errorf("beamdrop is not running")
	}
	peers := srv.Registry().All()
	if len(peers) == 0 {
		return 0, fmt.Errorf("nobody is connected to send to")
	}

	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		sent int
		last error
	)
	for _, e := range peers {
		wg.Add(1)
		go func(e *engine.Engine) {
			defer wg.Done()
			// One Sender per peer: it holds an open file handle and its own
			// read offset, so they cannot be shared across transfers.
			s, err := transfer.NewSender(path)
			if err != nil {
				mu.Lock()
				last = err
				mu.Unlock()
				return
			}
			defer s.Close()
			if err := e.SendFile(s, nil); err != nil {
				mu.Lock()
				last = fmt.Errorf("%s: %w", e.PeerName(), err)
				mu.Unlock()
				return
			}
			mu.Lock()
			sent++
			mu.Unlock()
		}(e)
	}
	wg.Wait()

	a.changed()
	if sent == 0 && last != nil {
		return 0, last
	}
	return sent, nil
}

// --- messages -------------------------------------------------------------

// MessageKind distinguishes the two things that travel between devices.
type MessageKind int

const (
	MessageText MessageKind = iota
	MessageFile
)

// Message is one entry in the history a window shows. Files that arrived
// are read back from the inbox rather than recorded here, so they survive a
// restart; text has nowhere else to live and is in memory only, which means
// it does not.
type Message struct {
	At       time.Time
	Peer     string
	Outbound bool
	Kind     MessageKind
	Text     string
	FileName string
	Size     int64
	Path     string // local path, for received files
}

const messageMax = 500

func (a *App) record(m Message) {
	a.mu.Lock()
	a.messages = append(a.messages, m)
	if len(a.messages) > messageMax {
		a.messages = append([]Message(nil), a.messages[len(a.messages)-messageMax:]...)
	}
	a.mu.Unlock()
	a.changed()
}

// Feed returns the history newest-last, merging remembered messages with
// the files actually sitting in the inbox. Reading received files off disk
// rather than trusting an in-memory list means the history is right even on
// the first run after a restart.
func (a *App) Feed(limit int) []Message {
	a.mu.Lock()
	out := append([]Message(nil), a.messages...)
	a.mu.Unlock()

	for _, f := range a.Inbox(0) {
		out = append(out, Message{
			At: f.Received, Kind: MessageFile, FileName: f.Name,
			Size: f.Size, Path: f.Path,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	if limit > 0 && len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out
}

// engineFor finds a connected peer by the name a window displays.
func (a *App) engineFor(peerName string) (*engine.Engine, error) {
	a.mu.Lock()
	srv := a.srv
	a.mu.Unlock()
	if srv == nil {
		return nil, fmt.Errorf("beamdrop is not running")
	}
	for _, e := range srv.Registry().All() {
		if e.PeerName() == peerName {
			return e, nil
		}
	}
	return nil, fmt.Errorf("%s is not connected", peerName)
}

// SendTextTo sends a message to one device.
func (a *App) SendTextTo(peerName, body string) error {
	if strings.TrimSpace(body) == "" {
		return fmt.Errorf("nothing to send")
	}
	e, err := a.engineFor(peerName)
	if err != nil {
		return err
	}
	if err := e.SendText(body); err != nil {
		return err
	}
	a.record(Message{At: time.Now(), Peer: peerName, Outbound: true, Kind: MessageText, Text: body})
	return nil
}

// SendFileTo sends a file to one device.
func (a *App) SendFileTo(peerName, path string) error {
	e, err := a.engineFor(peerName)
	if err != nil {
		return err
	}
	s, err := transfer.NewSender(path)
	if err != nil {
		return err
	}
	defer s.Close()
	if err := e.SendFile(s, nil); err != nil {
		return err
	}
	a.record(Message{
		At: time.Now(), Peer: peerName, Outbound: true, Kind: MessageFile,
		FileName: filepath.Base(path), Size: int64(s.Size), Path: path,
	})
	return nil
}

// firstLine keeps a multi-line message from taking over a one-line status.
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i] + "…"
	}
	if len(s) > 60 {
		s = s[:57] + "…"
	}
	return s
}

// SendTextToAll sends a message to every connected device.
func (a *App) SendTextToAll(body string) error {
	a.mu.Lock()
	srv := a.srv
	a.mu.Unlock()
	if srv == nil {
		return fmt.Errorf("beamdrop is not running")
	}
	peers := srv.Registry().All()
	if len(peers) == 0 {
		return fmt.Errorf("nobody is connected to send to")
	}
	var last error
	sent := 0
	for _, e := range peers {
		if err := e.SendText(body); err != nil {
			last = err
			continue
		}
		sent++
	}
	if sent == 0 {
		return last
	}
	a.record(Message{At: time.Now(), Outbound: true, Kind: MessageText, Text: body})
	return nil
}
