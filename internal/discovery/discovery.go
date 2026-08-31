// Package discovery finds a beamdrop portal on the local network without
// being told an address.
//
// The portal announces itself on demand rather than beaconing: it listens
// on UDP and answers probes. A one-shot `beamdrop send` gets a reply in
// milliseconds instead of waiting out a broadcast interval, and an idle
// laptop is not putting packets on the wire every two seconds forever.
//
// Broadcast does not traverse a tailnet, so this only solves the same-WiFi
// case. Reaching a laptop over Tailscale is handled by remembering a paired
// peer's address (peer.Peer.LastIP) and trying it directly — see
// mode.resolvePeer.
package discovery

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"strconv"
	"time"
)

// Port is the UDP port probes go to. It matches the TCP port on purpose —
// one number for the user to know — and does not collide, since UDP and TCP
// have separate port spaces.
const Port = 4747

const (
	magic0, magic1, magic2, magic3 = 'B', 'D', 'R', 'P'
	version                        = 1

	kindProbe    = 1
	kindAnnounce = 2

	// magic(4) + version(1) + kind(1) + port(2) + pubkey(32) + nameLen(1)
	headerLen = 41
)

// Peer is a portal that answered a probe.
type Peer struct {
	Name   string
	PubKey [32]byte
	Addr   string // host:port, ready to hand to net.Dial
}

// Self is what a responder says about itself.
type Self struct {
	Name   string
	PubKey [32]byte
	Port   int // the TCP port peers should connect to
}

func encode(kind byte, s Self) []byte {
	name := []byte(s.Name)
	if len(name) > 32 {
		name = name[:32]
	}
	out := make([]byte, headerLen+len(name))
	out[0], out[1], out[2], out[3] = magic0, magic1, magic2, magic3
	out[4] = version
	out[5] = kind
	binary.LittleEndian.PutUint16(out[6:8], uint16(s.Port))
	copy(out[8:40], s.PubKey[:])
	out[40] = byte(len(name))
	copy(out[headerLen:], name)
	return out
}

func decode(b []byte) (kind byte, s Self, err error) {
	if len(b) < headerLen {
		return 0, Self{}, errors.New("discovery: datagram too short")
	}
	if b[0] != magic0 || b[1] != magic1 || b[2] != magic2 || b[3] != magic3 {
		return 0, Self{}, errors.New("discovery: bad magic")
	}
	if b[4] != version {
		return 0, Self{}, fmt.Errorf("discovery: unsupported version %d", b[4])
	}
	nameLen := int(b[40])
	if headerLen+nameLen > len(b) {
		return 0, Self{}, errors.New("discovery: bad name length")
	}
	s.Port = int(binary.LittleEndian.Uint16(b[6:8]))
	copy(s.PubKey[:], b[8:40])
	s.Name = string(b[headerLen : headerLen+nameLen])
	return b[5], s, nil
}

// Responder answers discovery probes on behalf of a running portal.
type Responder struct {
	conn *net.UDPConn
	self Self
}

// Listen starts answering probes. addr is a UDP address such as ":4747";
// pass "127.0.0.1:0" in tests to get an ephemeral port.
func Listen(addr string, self Self) (*Responder, error) {
	ua, err := net.ResolveUDPAddr("udp4", addr)
	if err != nil {
		return nil, err
	}
	conn, err := net.ListenUDP("udp4", ua)
	if err != nil {
		return nil, err
	}
	r := &Responder{conn: conn, self: self}
	go r.serve()
	return r, nil
}

func (r *Responder) serve() {
	reply := encode(kindAnnounce, r.self)
	buf := make([]byte, 512)
	for {
		n, from, err := r.conn.ReadFromUDP(buf)
		if err != nil {
			return // closed
		}
		kind, _, err := decode(buf[:n])
		if err != nil || kind != kindProbe {
			continue // not ours, or malformed — say nothing
		}
		// Unicast back to the prober rather than broadcasting the reply.
		_, _ = r.conn.WriteToUDP(reply, from)
	}
}

func (r *Responder) Addr() net.Addr { return r.conn.LocalAddr() }
func (r *Responder) Close() error   { return r.conn.Close() }

// FindOptions controls a probe sweep.
type FindOptions struct {
	Port    int           // UDP port to probe; 0 means Port
	Timeout time.Duration // how long to collect replies; 0 means 1s
	// Targets are explicit destinations. Empty means every broadcast
	// address this host has, which is what finds a portal on the same WiFi.
	Targets []string
	// StopAfter returns as soon as this many portals have answered, instead
	// of sitting out the whole timeout. `beamdrop send` wants 1: waiting a
	// further second to see whether a second laptop replies helps nobody.
	StopAfter int
}

// Find probes for portals and returns whatever answered, deduplicated by
// public key. An empty result is not an error: nothing answering is the
// normal case when no portal is running nearby.
func Find(ctx context.Context, opts FindOptions) ([]Peer, error) {
	if opts.Port == 0 {
		opts.Port = Port
	}
	if opts.Timeout == 0 {
		opts.Timeout = time.Second
	}
	targets := opts.Targets
	if len(targets) == 0 {
		targets = broadcastAddrs()
	}
	if len(targets) == 0 {
		return nil, nil
	}

	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	probe := encode(kindProbe, Self{})
	for _, t := range targets {
		ua, err := net.ResolveUDPAddr("udp4", net.JoinHostPort(t, strconv.Itoa(opts.Port)))
		if err != nil {
			continue
		}
		// A failed send to one interface should not abandon the others.
		_, _ = conn.WriteToUDP(probe, ua)
	}

	deadline := time.Now().Add(opts.Timeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	if err := conn.SetReadDeadline(deadline); err != nil {
		return nil, err
	}

	seen := map[[32]byte]bool{}
	var found []Peer
	buf := make([]byte, 512)
	for {
		n, from, err := conn.ReadFromUDP(buf)
		if err != nil {
			break // deadline, or the socket went away
		}
		kind, s, err := decode(buf[:n])
		if err != nil || kind != kindAnnounce || seen[s.PubKey] {
			continue
		}
		seen[s.PubKey] = true
		found = append(found, Peer{
			Name:   s.Name,
			PubKey: s.PubKey,
			Addr:   net.JoinHostPort(from.IP.String(), strconv.Itoa(s.Port)),
		})
		if opts.StopAfter > 0 && len(found) >= opts.StopAfter {
			break
		}
		if ctx.Err() != nil {
			break
		}
	}
	return found, nil
}

// broadcastAddrs lists the per-interface broadcast addresses. 255.255.255.255
// is not used on its own: with several interfaces up the kernel picks one
// route for it, and that is often not the one the laptop is on.
func broadcastAddrs() []string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var out []string
	for _, ifi := range ifaces {
		if ifi.Flags&net.FlagUp == 0 || ifi.Flags&net.FlagBroadcast == 0 {
			continue
		}
		addrs, err := ifi.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok || ipnet.IP.To4() == nil {
				continue
			}
			if b := broadcastOf(ipnet); b != nil {
				out = append(out, b.String())
			}
		}
	}
	return out
}

func broadcastOf(n *net.IPNet) net.IP {
	ip := n.IP.To4()
	mask := net.IP(n.Mask).To4()
	if ip == nil || mask == nil {
		return nil
	}
	b := make(net.IP, 4)
	for i := range b {
		b[i] = ip[i] | ^mask[i]
	}
	return b
}
