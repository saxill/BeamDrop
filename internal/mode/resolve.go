package mode

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"time"

	"github.com/saxill/beamdrop/internal/discovery"
	"github.com/saxill/beamdrop/internal/pairing"
)

// resolveTimeout bounds the whole search. It is short on purpose: a
// one-shot `beamdrop send` that sits for ten seconds before reporting that
// it cannot find anything is worse than one that gives up and tells you to
// pass --peer.
const resolveTimeout = 2 * time.Second

// findPortal works out where to connect, in the order most likely to be
// right and cheapest to try:
//
//  1. An explicit --peer. Nothing guesses when the user has said.
//  2. A remembered peer's last address. This is the tailnet case — UDP
//     broadcast does not traverse a tailnet, so an address learned during a
//     previous pairing is the only thing that finds a laptop off-WiFi.
//  3. A broadcast probe. This is the same-WiFi case, and the one that means
//     a first-ever send needs no address at all.
//
// The returned string explains which path was taken, for printing.
func findPortal(host string, port int, known *pairing.KnownPeers) (addr, how string, err error) {
	if host != "" {
		return net.JoinHostPort(host, strconv.Itoa(port)), "", nil
	}

	if known != nil {
		if addr, name := lastReachable(known, port); addr != "" {
			return addr, fmt.Sprintf("remembered %s", name), nil
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), resolveTimeout)
	defer cancel()
	peers, ferr := discovery.Find(ctx, discovery.FindOptions{
		Port:      port,
		Timeout:   resolveTimeout,
		StopAfter: 1,
	})
	if ferr == nil && len(peers) > 0 {
		return peers[0].Addr, fmt.Sprintf("found %s on the local network", peers[0].Name), nil
	}

	// Loopback is worth one more try before giving up: running the portal in
	// another terminal on the same machine is the most common first thing
	// anyone does, and a probe to it can be lost to a firewall.
	local := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	if c, derr := net.DialTimeout("tcp", local, 300*time.Millisecond); derr == nil {
		c.Close()
		return local, "portal on this machine", nil
	}

	return "", "", fmt.Errorf("no portal found — start `beamdrop portal` on the other machine, or pass --peer <host>")
}

// lastReachable returns the first remembered peer whose address still
// accepts a connection, so a stale entry for a laptop that moved networks
// does not stall the send behind a full TCP timeout.
func lastReachable(known *pairing.KnownPeers, port int) (addr, name string) {
	for _, p := range known.All() {
		if p.LastIP == "" {
			continue
		}
		candidate := net.JoinHostPort(p.LastIP, strconv.Itoa(port))
		c, err := net.DialTimeout("tcp", candidate, 500*time.Millisecond)
		if err != nil {
			continue
		}
		c.Close()
		return candidate, p.Name
	}
	return "", ""
}
