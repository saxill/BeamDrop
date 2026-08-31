package mode

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/saxill/beamdrop/internal/discovery"
	"github.com/saxill/beamdrop/internal/engine"
	"github.com/saxill/beamdrop/internal/frame"
	"github.com/saxill/beamdrop/internal/netmux"
	"github.com/saxill/beamdrop/internal/pairing"
	"github.com/saxill/beamdrop/internal/push"
	"github.com/saxill/beamdrop/internal/spool"
	"github.com/saxill/beamdrop/internal/transfer"
	"github.com/saxill/beamdrop/internal/webui"
)

type peerServerOptions struct {
	ListenAddr string
	InboxDir   string
	ConfigDir  string
	Known      *pairing.KnownPeers
	// Identity is the portal's long-lived keypair. Without it the portal
	// presents a different public key on every connection, so a phone can
	// never recognise the laptop it paired with yesterday.
	Identity *pairing.KeyPair
	// Confirmer decides new pairings. When nil the server's own queue is
	// used, which is what lets a prompt be answered over the API by a front
	// end that is not this process.
	Confirmer engine.Confirmer
	// OnEngine, if set, is handed each peer's engine right after pairing,
	// before it starts serving. The desktop app uses it to attach a TEXT
	// handler per peer.
	OnEngine func(*engine.Engine)
	// UploadToken enables POST /upload for iOS Shortcuts. Empty disables
	// the endpoint entirely.
	UploadToken string
	// Spool, when set, turns this node into a relay: it accepts files
	// addressed to a peer that is offline and delivers them when that peer
	// comes back.
	Spool *spool.Spool
	// PeerPort is the port other nodes listen on. Zero means "the same one
	// we do", which is right whenever everybody uses the default.
	PeerPort int
	// DefaultTo is where uploads with no destination header are forwarded.
	DefaultTo string
	// ConnectTo, when set, makes this node dial the address and stay
	// connected to it. A laptop uses it to reach a relay it would otherwise
	// never hear from, so messages it sends can be passed on to the phone.
	ConnectTo string
	// Push, when set, notifies subscribed phones about arrivals. Nil simply
	// means no notifications, which is the right behaviour when the config
	// directory is unwritable or the feature was never set up.
	Push *push.Store
	// ForwardInterval overrides how often the spool is retried. Tests set
	// it short; production leaves it alone.
	ForwardInterval time.Duration
	// SpoolMaxAge drops undelivered files older than this.
	SpoolMaxAge time.Duration
	Logf        func(format string, args ...any)
}

// peerServer is the portal's network half, kept separate from the TUI so it
// can be tested without driving a terminal.
//
// Both kinds of peer land here and are treated identically from the
// engine's point of view: an iPhone arriving over WebSocket and a
// `beamdrop send` arriving over raw TCP each get their own responder
// engine, registered under their public key.
type peerServer struct {
	opts      peerServerOptions
	activity  *activityLog
	messages  *messageLog
	pairs     *pairQueue
	registry  *engine.Registry
	mux       *netmux.Listener
	responder *discovery.Responder
}

func startPeerServer(ctx context.Context, opts peerServerOptions) (*peerServer, error) {
	if opts.Logf == nil {
		opts.Logf = func(string, ...any) {}
	}
	// Everything logged also feeds the dashboard, which is the only view a
	// headless relay has.
	activity := newActivityLog(opts.Logf)
	opts.Logf = activity.Logf
	// NewReceiver opens files with O_CREATE, which does not create parent
	// directories — so without this the first file a phone sends to a fresh
	// install fails, and it fails on the receiving side where nobody is
	// looking.
	if opts.InboxDir != "" {
		if err := os.MkdirAll(opts.InboxDir, 0o755); err != nil {
			return nil, fmt.Errorf("portal: inbox: %w", err)
		}
	}

	base, err := net.Listen("tcp", opts.ListenAddr)
	if err != nil {
		return nil, fmt.Errorf("portal: listen on %s: %w", opts.ListenAddr, err)
	}

	s := &peerServer{
		opts:     opts,
		registry: engine.NewRegistry(),
		activity: activity,
		messages: newMessageLog(),
		pairs:    newPairQueue(),
	}
	s.mux = netmux.Listen(base, func(c net.Conn) {
		defer c.Close()
		if err := s.servePeer(c, false); err != nil {
			opts.Logf("peer %s: %v", c.RemoteAddr(), err)
		}
	})

	// Answer discovery probes so `beamdrop send` on the same WiFi does not
	// need to be told an address. Best-effort: something else already bound
	// to the UDP port is a reason to skip discovery, not to refuse to start
	// the portal.
	tcpPort := s.mux.Addr().(*net.TCPAddr).Port
	if r, derr := discovery.Listen(fmt.Sprintf(":%d", tcpPort), discovery.Self{
		Name:   hostName(),
		PubKey: identityPub(opts.Identity),
		Port:   tcpPort,
	}); derr == nil {
		s.responder = r
	} else {
		opts.Logf("discovery unavailable: %v", derr)
	}

	// Anyone who types "host:4747" into a browser arrives here over plain
	// HTTP. Send them to the https URL rather than dropping the connection,
	// which a browser shows as "cannot open the page" — the same thing it
	// shows when nothing is listening at all.
	plainMux := http.NewServeMux()
	if opts.UploadToken != "" {
		rt := uploadRoute{
			InboxDir:   opts.InboxDir,
			Token:      opts.UploadToken,
			SelfName:   hostName(),
			Spool:      opts.Spool,
			KnownNames: func() []string { return knownPeerNames(opts.Known) },
			Logf:       opts.Logf,
		}
		// The dashboard and downloads sit behind the same door as uploads:
		// tailnet-only, same token, plain HTTP so there is no certificate to
		// click through on a machine with no screen.
		plainMux.Handle("/status", dashboardHandler(rt, s.registry, opts.Spool, activity))
		plainMux.Handle("/files", filesHandler(rt))
		// The API is loopback-only (checked inside), so a front end in any
		// language can drive this machine without opening it to the tailnet
		// the way /upload is.
		plainMux.Handle("/api/", apiHandler(apiOptions{
			Token:    opts.UploadToken,
			InboxDir: opts.InboxDir,
			Registry: s.registry,
			Spool:    opts.Spool,
			Pairs:    s.pairs,
			Activity: activity,
			Messages: s.messages,
			Port:     tcpPort,
			Logf:     opts.Logf,
		}))
	}
	// Plain HTTP because Shortcuts will not tap through a self-signed
	// certificate, and tailnet traffic is already WireGuard-encrypted. The
	// same handler is mounted on the TLS side below: the MagicDNS name now
	// carries a real Let's Encrypt certificate, and https is the address
	// people are given for the phone, so it has to take a file too.
	var uploadTLS map[string]http.Handler
	if opts.UploadToken != "" {
		up := uploadHandlerRouted(uploadRoute{
			InboxDir:   opts.InboxDir,
			Token:      opts.UploadToken,
			SelfName:   hostName(),
			Spool:      opts.Spool,
			KnownNames: func() []string { return knownPeerNames(opts.Known) },
			DefaultTo:  opts.DefaultTo,
			Logf:       opts.Logf,
		})
		plainMux.Handle("/upload", up)
		uploadTLS = map[string]http.Handler{"/upload": up}
	}
	plainMux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if host == "" {
			host = s.mux.Addr().String()
		}
		http.Redirect(w, r, "https://"+host+r.URL.RequestURI(), http.StatusPermanentRedirect)
	})
	go func() {
		plain := &http.Server{Handler: plainMux}
		go func() { <-ctx.Done(); plain.Close() }()
		_ = plain.Serve(s.mux.PlainHTTP())
	}()

	// The relay half: deliver anything spooled for a peer that was offline
	// when it arrived, as soon as that peer is reachable again.
	if opts.Spool != nil {
		peerPort := opts.PeerPort
		if peerPort == 0 {
			peerPort = tcpPort
		}
		f := &forwarder{
			spool:    opts.Spool,
			known:    opts.Known,
			identity: opts.Identity,
			registry: s.registry,
			port:     peerPort,
			interval: opts.ForwardInterval,
			maxAge:   opts.SpoolMaxAge,
			logf:     opts.Logf,
		}
		go f.Run(ctx)
	}

	// The outbound half of a relay: a laptop that would otherwise never hear
	// from the relay dials it here and stays connected, so it has a peer to
	// send to. Re-dial on disconnect — the relay may be rebooting, and the
	// whole point is that the laptop keeps a path into it.
	if opts.ConnectTo != "" {
		go func() {
			for {
				if ctx.Err() != nil {
					return
				}
				if err := s.dialPeer(opts.ConnectTo); err != nil {
					opts.Logf("connect to %s: %v", opts.ConnectTo, err)
				}
				select {
				case <-ctx.Done():
					return
				case <-time.After(5 * time.Second):
				}
			}
		}()
	}

	go func() {
		err := webui.Serve(ctx, webui.ServeOptions{
			CertDir:  opts.ConfigDir,
			Listener: s.mux,
			Routes:   uploadTLS,
			OnConnect: func(_ context.Context, conn net.Conn) error {
				return s.servePeer(conn, false)
			},
		})
		if err != nil && ctx.Err() == nil && !errors.Is(err, net.ErrClosed) {
			opts.Logf("webui stopped: %v", err)
		}
	}()

	return s, nil
}

// servePeer pairs over conn and then runs that peer's receive loop until it
// disconnects. It blocks for the lifetime of the connection. isInitiator is
// true when this node dialled out (a laptop reaching a relay) rather than
// accepting an inbound connection (a phone or `beamdrop send`).
func (s *peerServer) servePeer(conn net.Conn, isInitiator bool) error {
	e, err := engine.New(engine.Config{
		Name:        hostName(),
		Conn:        conn,
		IsInitiator: isInitiator,
		Identity:    s.opts.Identity,
		Known:       s.opts.Known,
		Confirmer:   s.confirmer(),
		OnReceiveError: func(err error) {
			s.opts.Logf("receive failed: %v", err)
		},
	})
	if err != nil {
		return err
	}
	e.OnFileOffer(func(frame.FileOfferPayload) (string, bool) {
		// The inbox directory only — never a path built from offer.Name.
		// See engine.FileAcceptor.
		return s.opts.InboxDir, true
	})
	name := e.PeerName()
	peerKey := hex.EncodeToString(keyBytes(e.PeerPubKey()))
	e.OnText(func(body string) {
		s.messages.record(Message{At: time.Now(), Peer: name, Kind: MessageText, Text: body})
		s.opts.Logf("%s: %s", name, firstLine(body))
		s.notify(push.Notification{
			Title: name,
			Body:  firstLine(body),
			Tag:   "text",
		}, peerKey)
		s.relay(name,
			func(e *engine.Engine) error { return e.SendText(body) },
			func(to string) error {
				_, err := s.opts.Spool.AddText(to, body)
				return err
			})
	})
	e.OnHistoryRequest(func() []frame.HistoryEntry {
		return s.historyFor(name)
	})
	e.OnPushKeyRequest(func() string {
		if s.opts.Push == nil {
			return ""
		}
		return s.opts.Push.PublicKey()
	})
	e.OnPushSubscribe(func(p frame.PushSubscribePayload) {
		if s.opts.Push == nil {
			return
		}
		if err := s.opts.Push.Add(push.Subscription{
			Peer:     name,
			PeerKey:  peerKey,
			Endpoint: p.Endpoint,
			P256dh:   p.P256dh,
			Auth:     p.Auth,
		}); err != nil {
			s.opts.Logf("push: could not register %s: %v", name, err)
			return
		}
		s.opts.Logf("%s will be notified when something arrives", name)
	})
	e.OnFileRequest(func(want string) {
		path, err := s.inboxFile(want)
		if err != nil {
			s.opts.Logf("%s asked for %q: %v", name, want, err)
			return
		}
		sender, err := transfer.NewSender(path)
		if err != nil {
			s.opts.Logf("%s asked for %q: %v", name, want, err)
			return
		}
		defer sender.Close()
		if err := e.SendFile(sender, nil); err != nil {
			s.opts.Logf("sending %s to %s: %v", want, name, err)
			return
		}
		s.opts.Logf("sent %s back to %s", want, name)
	})
	e.OnFileReceived(func(fileName, path string) {
		// The desktop is told here rather than only in writeToInbox: that
		// path is the Shortcut's, so a file sent from the phone's own page —
		// or handed over by the relay — used to land in the inbox with
		// nothing on screen to say it had.
		var size int64
		if st, err := os.Stat(path); err == nil {
			size = st.Size()
		}
		notifyArrival(fileName, size)
		s.notify(push.Notification{
			Title: name + " sent a file",
			Body:  fileName,
			Tag:   "file",
		}, peerKey)
		s.relay(name,
			func(e *engine.Engine) error {
				sender, err := transfer.NewSender(path)
				if err != nil {
					return err
				}
				defer sender.Close()
				return e.SendFile(sender, nil)
			},
			func(to string) error {
				f, err := os.Open(path)
				if err != nil {
					return err
				}
				defer f.Close()
				_, err = s.opts.Spool.Add(to, fileName, f)
				return err
			})
	})
	if s.opts.OnEngine != nil {
		s.opts.OnEngine(e)
	}
	s.registry.Add(e)
	defer s.registry.Remove(e.PeerPubKey())
	s.opts.Logf("%s connected", e.PeerName())

	err = e.Serve()
	s.opts.Logf("%s disconnected", e.PeerName())
	if engine.IsDisconnect(err) {
		return nil
	}
	return err
}

// dialPeer connects out to a relay and serves that connection as the
// initiator. It blocks until the connection ends, so the caller's reconnect
// loop can simply call it again.
func (s *peerServer) dialPeer(addr string) error {
	conn, err := net.DialTimeout("tcp", addr, forwardDialTimeout)
	if err != nil {
		return err
	}
	defer conn.Close()
	return s.servePeer(conn, true)
}

// inboxFile resolves a name a peer asked for to a real file in the inbox,
// or refuses.
//
// The name is entirely peer-controlled, so it is reduced to its base
// component before it is joined to anything — the same rule as
// engine.FileAcceptor, and for the same reason. filepath.Join *cleans*
// "../" rather than rejecting it, so a joined path can legitimately land
// outside the inbox. Reducing to the base first is what makes that
// impossible rather than merely unlikely.
//
// The resolved path is then checked to be a regular file directly inside
// the inbox, so a symlink planted there cannot be used to read elsewhere.
func (s *peerServer) inboxFile(want string) (string, error) {
	base := filepath.Base(want)
	if base == "." || base == string(filepath.Separator) || base == ".." {
		return "", fmt.Errorf("not a file name")
	}
	dir, err := filepath.EvalSymlinks(s.opts.InboxDir)
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, base)
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	if filepath.Dir(resolved) != dir {
		return "", fmt.Errorf("%q resolves outside the inbox", base)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("%q is not a regular file", base)
	}
	return resolved, nil
}

// keyBytes exists only to take the address of an array so it can be sliced.
func keyBytes(k [32]byte) []byte { return k[:] }

// notify tells every subscribed device except the one that caused the
// event. It runs detached because a push is an HTTPS round trip to a
// vendor's service on the public internet: doing it inline would stall the
// frame reader behind a network the local transfer does not depend on.
func (s *peerServer) notify(n push.Notification, exceptPeerKey string) {
	if s.opts.Push == nil {
		return
	}
	go func() {
		for _, err := range s.opts.Push.Notify(n, exceptPeerKey) {
			s.opts.Logf("push: %v", err)
		}
	}()
}

// historyMax is what a reconnecting phone is sent. Enough to recognise
// where you left off, small enough that a cellular connection is not made
// to pull a wall of filenames before the page can draw anything.
const historyMax = 50

// historyFor builds the answer to a peer's history request.
//
// asker is the peer's name, which is what decides the Outbound flag: a file
// this machine received *from* the asker is something the asker sent, and
// has to appear on their side of the conversation, not ours. Getting that
// backwards would show a phone its own photos as though the laptop had sent
// them.
//
// A paired peer sees everything in the feed, not only its own traffic. That
// is deliberate — the whole point is one conversation with this machine
// rather than a separate one per device — but it does mean pairing hands
// over the inbox's filenames, which is worth knowing before pairing
// something you do not control.
func (s *peerServer) historyFor(asker string) []frame.HistoryEntry {
	msgs := s.messages.feed(s.opts.InboxDir, historyMax)
	out := make([]frame.HistoryEntry, 0, len(msgs))
	for _, m := range msgs {
		e := frame.HistoryEntry{
			At:   m.At.Unix(),
			Peer: m.Peer,
			// Received from the asker, or sent by us to them: either way it
			// is on the asker's side of the conversation if they are the
			// one who sent it.
			Outbound: !m.Outbound && m.Peer != "" && strings.EqualFold(m.Peer, asker),
		}
		switch m.Kind {
		case MessageText:
			e.Kind, e.Text = "text", m.Text
		case MessageFile:
			e.Kind, e.Name, e.Size = "file", m.FileName, m.Size
		default:
			continue
		}
		out = append(out, e)
	}
	return out
}

// relay passes something that just arrived on to the machine this relay
// exists to serve, so it stops mattering which of the two addresses the
// phone happened to open.
//
// Without it a relay is a dead end for anything sent from its own page:
// only POST /upload ever reached the spool, so a photo dropped on the Pi's
// page landed in the Pi's inbox and stayed there, while the laptop — the
// machine you actually wanted it on — showed nothing.
//
// It is also the reverse half of the relay: something that arrived *from*
// the destination (the laptop dialling in) is broadcast to every other
// connected peer (the phone), so a message sent on the laptop reaches the
// phone through the relay instead of dying at the laptop's empty peer list.
//
// queue runs only on a relay that has been told where things go by default.
// Failing to spool is logged rather than returned: the file is already
// safely in this machine's inbox, so a copy that could not be made is a
// degradation, not a lost file, and must not break the live connection.
func (s *peerServer) relay(fromPeer string, send func(*engine.Engine) error, queue func(to string) error) {
	to := s.opts.DefaultTo
	if s.opts.Spool == nil || to == "" {
		return
	}
	// Something that came *from* the destination is not spooled back to it —
	// that is a loop, and on a relay whose whole job is retrying, a loop is
	// not a glitch, it is an inbox filling up forever. Instead it is the
	// laptop talking: hand it to every other connected peer.
	if strings.EqualFold(fromPeer, to) {
		for _, e := range s.registry.All() {
			if strings.EqualFold(e.PeerName(), fromPeer) {
				continue
			}
			if err := send(e); err != nil {
				s.opts.Logf("relay to %s: %v", e.PeerName(), err)
			}
		}
		return
	}
	if err := queue(to); err != nil {
		s.opts.Logf("could not pass on to %s: %v", to, err)
		return
	}
	s.opts.Logf("passing on to %s", to)
}

// confirmer prefers whatever the caller supplied, falling back to the
// queue so an API-driven front end still gets asked.
func (s *peerServer) confirmer() engine.Confirmer {
	if s.opts.Confirmer != nil {
		return s.opts.Confirmer
	}
	return func(_, _ [32]byte, peerName, code string, isKnown bool) bool {
		if isKnown {
			return true
		}
		s.opts.Logf("%s wants to connect (code %s)", peerName, code)
		return s.pairs.Ask(peerName, code)
	}
}

func (s *peerServer) Registry() *engine.Registry { return s.registry }
func (s *peerServer) Addr() net.Addr             { return s.mux.Addr() }

func (s *peerServer) Close() error {
	// Anything parked waiting for a human gets a no, or its engine
	// goroutine stays blocked forever.
	s.pairs.RejectAll()
	if s.responder != nil {
		s.responder.Close()
	}
	return s.mux.Close()
}

func identityPub(kp *pairing.KeyPair) [32]byte {
	if kp == nil {
		return [32]byte{}
	}
	return kp.Pub
}
