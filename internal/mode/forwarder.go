package mode

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/saxill/beamdrop/internal/engine"
	"github.com/saxill/beamdrop/internal/pairing"
	"github.com/saxill/beamdrop/internal/spool"
	"github.com/saxill/beamdrop/internal/transfer"
)

// The forwarder is the half of the relay that runs after the sender has
// gone. A phone hands a file to an always-on machine, that machine tells
// the phone it is delivered, and from then on getting it to a laptop that
// may be shut for days is entirely this loop's problem.
//
// Which is why nothing here deletes a spooled file until the destination
// has confirmed its SHA-256. The relay holds the only copy.

const (
	// forwardInterval is how often the backlog is retried. Seconds would be
	// wasteful against a laptop that is off for a week, and minutes would
	// make "open the lid and the file appears" feel broken. Discovery of a
	// peer coming back is cheap — a dial to a remembered address.
	forwardInterval = 2 * time.Second

	// forwardDialTimeout is short because the common failure is "that
	// machine is off", and the loop has other items to try.
	forwardDialTimeout = 3 * time.Second
)

type forwarder struct {
	spool    *spool.Spool
	known    *pairing.KnownPeers
	identity *pairing.KeyPair
	// registry, when set, lets the forwarder deliver over a connection the
	// destination already has open instead of dialling a second one. Without
	// it, a laptop that dials a relay (--connect-to) would be dialled back
	// by the forwarder for every spooled message, and that second connection
	// — same public key — would evict the persistent one from the registry.
	registry *engine.Registry
	// port is where *other* beamdrop nodes listen. Pairing cannot teach us
	// this: the connection a peer opens to us has an ephemeral source port,
	// not its listening one. Everything in beamdrop uses one well-known
	// port, so defaulting to our own is right — but it is an assumption, so
	// it is a field rather than a hard-coded value.
	port     int
	interval time.Duration
	// maxAge drops files nobody ever came back for. Zero keeps everything,
	// which is the right default for a laptop but wrong for a Pi.
	maxAge time.Duration
	logf   func(string, ...any)
}

// Run drains the spool until ctx is cancelled. It sweeps once immediately
// so a relay that restarts with a backlog does not wait out a full interval
// before trying.
func (f *forwarder) Run(ctx context.Context) {
	every := f.interval
	if every <= 0 {
		every = forwardInterval
	}
	t := time.NewTicker(every)
	defer t.Stop()
	f.sweep(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			f.sweep(ctx)
		}
	}
}

func (f *forwarder) sweep(ctx context.Context) {
	// Expire before delivering, so a stale backlog is not retried forever.
	// Say so out loud: the sender was told these files were accepted, so
	// them vanishing has to be visible somewhere.
	if dropped, err := f.spool.Expire(f.maxAge); err == nil {
		for _, d := range dropped {
			f.logf("gave up on %s for %s after %s", d.Name, d.To, time.Since(d.ReceivedAt).Round(time.Hour))
		}
	}

	items, err := f.spool.Pending()
	if err != nil {
		f.logf("spool: %v", err)
		return
	}
	// Peers that are plainly unreachable are skipped for the rest of this
	// sweep rather than dialled once per queued file — a hundred photos for
	// a laptop that is off should cost one timeout, not a hundred.
	unreachable := map[string]bool{}
	for _, item := range items {
		if ctx.Err() != nil {
			return
		}
		if unreachable[item.To] {
			continue
		}
		if err := f.deliver(item); err != nil {
			unreachable[item.To] = true
			_ = f.spool.Failed(item, err)
			continue
		}
		if err := f.spool.Done(item); err != nil {
			f.logf("spool: could not clear %s: %v", item.Name, err)
			continue
		}
		f.logf("delivered %s to %s", item.Name, item.To)
	}
}

// deliver sends one spooled item, returning nil only once the destination
// has confirmed the file's hash — or, for a message, once it has been
// written to the wire.
func (f *forwarder) deliver(item spool.Item) error {
	// If the destination is already connected, hand it over that live
	// connection rather than dialling a second one. Dialling a peer that is
	// already here would register a second engine under the same public key
	// and evict the persistent connection from the registry — which is how a
	// laptop that dials a relay kept losing it every time a spooled message
	// arrived.
	if e := f.connectedEngine(item.To); e != nil {
		return f.sendOver(e, item)
	}

	addr, err := f.addrFor(item.To)
	if err != nil {
		return err
	}
	conn, err := net.DialTimeout("tcp", addr, forwardDialTimeout)
	if err != nil {
		return fmt.Errorf("dial %s: %w", addr, err)
	}
	defer conn.Close()

	e, err := f.dialEngine(conn)
	if err != nil {
		return err
	}
	return f.sendOver(e, item)
}

// sendOver writes one spooled item to an engine that is already paired with
// the destination. A message has no file to open and no hash to await, so it
// is set up and finished differently from a file.
func (f *forwarder) sendOver(e *engine.Engine, item spool.Item) error {
	if item.Kind == spool.KindText {
		body, err := f.spool.Text(item)
		if err != nil {
			return err
		}
		return e.SendText(body)
	}
	s, err := transfer.NewSenderNamed(item.Path(), item.Name)
	if err != nil {
		return err
	}
	defer s.Close()
	return e.SendFile(s, nil)
}

// connectedEngine returns the live engine for a destination that is already
// connected to this node, or nil if it is not. The registry is keyed by
// public key, so the name has to be matched by hand.
func (f *forwarder) connectedEngine(name string) *engine.Engine {
	if f.registry == nil {
		return nil
	}
	for _, e := range f.registry.All() {
		if strings.EqualFold(e.PeerName(), name) {
			return e
		}
	}
	return nil
}

// dialEngine pairs with the destination over an already-open connection.
func (f *forwarder) dialEngine(conn net.Conn) (*engine.Engine, error) {
	return engine.New(engine.Config{
		Name:        hostName(),
		Conn:        conn,
		IsInitiator: true,
		Identity:    f.identity,
		Known:       f.known,
		// A relay runs unattended, so there is nobody to confirm a code.
		// Only an already-known peer is acceptable: pairing with a stranger
		// without a human present is exactly what TOFU is supposed to
		// prevent.
		Confirmer: func(_, _ [32]byte, _, _ string, isKnown bool) bool { return isKnown },
	})
}

// addrFor resolves a destination peer name to a dialable address using the
// address remembered from the last time it was seen.
func (f *forwarder) addrFor(name string) (string, error) {
	if f.known == nil {
		return "", fmt.Errorf("no known-peers store")
	}
	for _, p := range f.known.All() {
		if p.Name != name {
			continue
		}
		if p.LastIP == "" {
			return "", fmt.Errorf("peer %q has no remembered address yet", name)
		}
		return net.JoinHostPort(p.LastIP, strconv.Itoa(f.port)), nil
	}
	return "", fmt.Errorf("peer %q is not paired with this relay", name)
}

// knownPeerNames lists who this node can forward to, for error messages
// and for rejecting an upload aimed at a stranger.
func knownPeerNames(known *pairing.KnownPeers) []string {
	if known == nil {
		return nil
	}
	var out []string
	for _, p := range known.All() {
		if p.Name != "" {
			out = append(out, p.Name)
		}
	}
	return out
}
