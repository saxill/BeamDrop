package mode

import (
	"context"
	"fmt"
	"net"
	"path/filepath"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/saxill/beamdrop/internal/engine"
	"github.com/saxill/beamdrop/internal/pairing"
	"github.com/saxill/beamdrop/internal/peer"
	"github.com/saxill/beamdrop/internal/push"
	"github.com/saxill/beamdrop/internal/spool"
	"github.com/saxill/beamdrop/internal/transfer"
	"github.com/saxill/beamdrop/internal/webui"
)

type PortalOptions struct {
	InboxDir      string
	KnownPeersDir string
	// ConfigDir is where the web UI's TLS keypair is kept so the phone
	// does not have to re-accept a new certificate on every restart.
	// Empty means use an ephemeral certificate.
	ConfigDir string
	Port      int
	// Relay turns this node into a store-and-forward hub: it accepts files
	// addressed to peers that are offline and delivers them later. Meant
	// for a machine that stays up — a Pi, a server — so a phone always has
	// somewhere to hand a file even when the laptop is shut.
	Relay bool
	// RelayTo is the peer an unaddressed upload is forwarded to. Without it
	// a relay keeps everything, which is rarely what a relay is for.
	RelayTo string
	// ConnectTo, when set, makes this node dial the given address and stay
	// connected to it. A laptop uses it to reach a relay it would otherwise
	// never hear from, so messages it sends can be passed on to the phone.
	ConnectTo string
	// SpoolMaxAge drops relayed files nobody collected. Zero means the
	// package default.
	SpoolMaxAge time.Duration
}

type pairRequest struct {
	name    string
	code    string
	known   bool
	respond chan bool
}

type sendResult struct {
	peer string
	path string
	err  error
}

type logMsg string

type model struct {
	inboxDir string
	input    string
	log      []string
	quitting bool

	registry *engine.Registry
	pairCh   <-chan pairRequest
	// resultCh stays bidirectional (not <-chan sendResult): Update
	// spawns sendToPeer goroutines that send into this same channel
	// instance, so the model needs to hand it to them as-is rather
	// than narrowing it to receive-only.
	resultCh chan sendResult
	logCh    <-chan string
	pending  *pairRequest
}

func initialModel(inboxDir string, registry *engine.Registry, pairCh <-chan pairRequest, resultCh chan sendResult, logCh <-chan string) model {
	return model{
		inboxDir: inboxDir,
		registry: registry,
		pairCh:   pairCh,
		resultCh: resultCh,
		logCh:    logCh,
		log:      []string{"beamdrop portal — type :send /path/to/file or :q to quit"},
	}
}

func waitForPair(ch <-chan pairRequest) tea.Cmd {
	return func() tea.Msg {
		req, ok := <-ch
		if !ok {
			return nil
		}
		return req
	}
}

func waitForResult(ch <-chan sendResult) tea.Cmd {
	return func() tea.Msg {
		res, ok := <-ch
		if !ok {
			return nil
		}
		return res
	}
}

func waitForLog(ch <-chan string) tea.Cmd {
	return func() tea.Msg {
		msg, ok := <-ch
		if !ok {
			return nil
		}
		return logMsg(msg)
	}
}

func (m model) Init() tea.Cmd {
	return tea.Batch(waitForPair(m.pairCh), waitForResult(m.resultCh), waitForLog(m.logCh))
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case pairRequest:
		m.pending = &msg
		m.log = append(m.log, fmt.Sprintf("pairing: %s wants to connect (code %s) — [y/n]", msg.name, msg.code))
		return m, nil
	case sendResult:
		if msg.err != nil {
			m.log = append(m.log, fmt.Sprintf("send %s to %s failed: %v", msg.path, msg.peer, msg.err))
		} else {
			m.log = append(m.log, fmt.Sprintf("sent %s to %s", msg.path, msg.peer))
		}
		return m, waitForResult(m.resultCh)
	case logMsg:
		m.log = append(m.log, string(msg))
		return m, waitForLog(m.logCh)
	case tea.KeyMsg:
		if m.pending != nil {
			switch msg.String() {
			case "y":
				m.pending.respond <- true
				m.log = append(m.log, "paired.")
				m.pending = nil
				return m, waitForPair(m.pairCh)
			case "n":
				m.pending.respond <- false
				m.log = append(m.log, "rejected.")
				m.pending = nil
				return m, waitForPair(m.pairCh)
			}
			return m, nil
		}
		switch msg.String() {
		case "ctrl+c", ":q":
			m.quitting = true
			return m, tea.Quit
		case "enter":
			line := strings.TrimSpace(m.input)
			m.input = ""
			if line == "" {
				return m, nil
			}
			if strings.HasPrefix(line, ":send ") {
				path := strings.TrimPrefix(line, ":send ")
				peers := m.registry.All()
				if len(peers) == 0 {
					m.log = append(m.log, "no peers connected")
				} else {
					m.log = append(m.log, fmt.Sprintf("sending %s to %d peer(s)…", path, len(peers)))
					for _, e := range peers {
						go sendToPeer(e, path, m.resultCh)
					}
				}
			} else {
				m.log = append(m.log, "unknown: "+line)
			}
		case "backspace":
			if len(m.input) > 0 {
				m.input = m.input[:len(m.input)-1]
			}
		default:
			if len(msg.String()) == 1 {
				m.input += msg.String()
			}
		}
	}
	return m, nil
}

func (m model) View() string {
	if m.quitting {
		return ""
	}
	header := lipgloss.NewStyle().Bold(true).Render(fmt.Sprintf("beamdrop · inbox: %s", m.inboxDir))
	logBox := strings.Join(m.log, "\n")
	prompt := fmt.Sprintf("\n> %s", m.input)
	return fmt.Sprintf("%s\n\n%s%s", header, logBox, prompt)
}

func sendToPeer(e *engine.Engine, path string, results chan<- sendResult) {
	s, err := transfer.NewSender(path)
	if err != nil {
		results <- sendResult{peer: e.PeerName(), path: path, err: err}
		return
	}
	defer s.Close()
	err = e.SendFile(s, nil)
	results <- sendResult{peer: e.PeerName(), path: path, err: err}
}

func Portal(opts PortalOptions) error {
	known, err := pairing.NewKnownPeers(opts.KnownPeersDir, peer.NewMemStore())
	if err != nil {
		return err
	}
	identity, err := pairing.LoadOrCreateIdentity(opts.ConfigDir)
	if err != nil {
		return err
	}
	pairCh := make(chan pairRequest)
	resultCh := make(chan sendResult, 8)
	logCh := make(chan string, 64)

	// Dropping a log line is preferable to wedging the goroutine that
	// produced it: connection handlers and the engine's receive-error hook
	// both call this, and the TUI only drains one message per Update.
	logf := func(format string, args ...any) {
		select {
		case logCh <- fmt.Sprintf(format, args...):
		default:
		}
	}

	// Without a terminal there is no TUI to prompt in, so the confirmer has
	// to decide on its own — see headlessConfirmer for why that is
	// acceptable and what carries the weight instead.
	headless := !hasTerminal()
	confirmer := headlessConfirmer(logf)
	if !headless {
		confirmer = func(initPub, respPub [32]byte, peerName, code string, isKnown bool) bool {
			if isKnown {
				return true
			}
			req := pairRequest{name: peerName, code: code, known: isKnown, respond: make(chan bool, 1)}
			pairCh <- req
			return <-req.respond
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Best-effort: an unwritable config dir should cost you the Shortcuts
	// endpoint, not the portal.
	uploadToken, terr := loadOrCreateUploadToken(opts.ConfigDir)
	if terr != nil {
		uploadToken = ""
	}

	if opts.SpoolMaxAge == 0 {
		opts.SpoolMaxAge = spool.DefaultMaxAge
	}
	var sp *spool.Spool
	if opts.Relay {
		sp, err = spool.Open(filepath.Join(opts.ConfigDir, "spool"))
		if err != nil {
			return err
		}
	}

	// Best-effort, like the upload token: an unwritable config dir should
	// cost you notifications, not the portal. A nil store simply means the
	// page is never offered a "turn on notifications" button.
	pushStore, perr := push.Open(filepath.Join(opts.ConfigDir, "push"), "")
	if perr != nil {
		logf("notifications unavailable: %v", perr)
		pushStore = nil
	}

	srv, err := startPeerServer(ctx, peerServerOptions{
		ListenAddr:  fmt.Sprintf(":%d", opts.Port),
		Spool:       sp,
		Push:        pushStore,
		DefaultTo:   opts.RelayTo,
		ConnectTo:   opts.ConnectTo,
		SpoolMaxAge: opts.SpoolMaxAge,
		InboxDir:    opts.InboxDir,
		ConfigDir:   opts.ConfigDir,
		Known:       known,
		Identity:    &identity,
		Confirmer:   confirmer,
		UploadToken: uploadToken,
		Logf:        logf,
	})
	if err != nil {
		return err
	}
	defer srv.Close()

	banner := reachableURLs(opts.Port)

	m := initialModel(opts.InboxDir, srv.Registry(), pairCh, resultCh, logCh)
	m.log = append(m.log, banner...)
	if uploadToken != "" && terr == nil {
		m.log = append(m.log, shortcutHint(opts.Port, opts.ConfigDir))
	}
	if sp != nil {
		pending, _ := sp.Pending()
		m.log = append(m.log, fmt.Sprintf("relay mode: holding files for offline peers (%d waiting)", len(pending)))
	}

	if headless {
		full := append([]string{fmt.Sprintf("beamdrop: inbox %s", opts.InboxDir)}, banner...)
		if uploadToken != "" && terr == nil {
			full = append(full, shortcutHint(opts.Port, opts.ConfigDir))
		}
		if sp != nil {
			pending, _ := sp.Pending()
			full = append(full, fmt.Sprintf("relay mode: holding files for offline peers (%d waiting)", len(pending)))
		}
		return runHeadless(ctx, opts, logCh, full)
	}

	p := tea.NewProgram(m)
	_, err = p.Run()
	return err
}

// reachableURLs lists the addresses to open on the phone. Printing them is
// the difference between the portal working and the user guessing: the
// address that matters is the Tailscale one, and nothing else on screen
// tells them what it is.
func reachableURLs(port int) []string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil
	}
	var tailscale, lan []string
	for _, a := range addrs {
		ipnet, ok := a.(*net.IPNet)
		if !ok || ipnet.IP.IsLoopback() || ipnet.IP.IsLinkLocalUnicast() {
			continue
		}
		ip4 := ipnet.IP.To4()
		if ip4 == nil {
			continue // IPv6 URLs need brackets and are rarely what you want here
		}
		url := fmt.Sprintf("  https://%s:%d", ip4, port)
		// Tailscale hands out 100.64.0.0/10, the CGNAT range. That is the
		// address that works from cellular, so it goes first.
		if ip4[0] == 100 && ip4[1] >= 64 && ip4[1] <= 127 {
			tailscale = append(tailscale, url+"   (tailnet — works off-WiFi)")
			continue
		}
		lan = append(lan, url+"   (same WiFi)")
	}
	// The MagicDNS name goes above all of them when Tailscale can issue a
	// certificate for it, because it is the only address that opens without
	// a security warning — the certificate is a real, publicly trusted one.
	// Every IP below it still works, but costs a tap-through.
	var out []string
	if domain := webui.CertDomain(); domain != "" {
		out = append(out, fmt.Sprintf("  https://%s:%d   (no warning — use this one)", domain, port))
	}
	out = append(out, tailscale...)
	out = append(out, lan...)
	if len(out) == 0 {
		return []string{"no non-loopback address found — is the network up?"}
	}
	return append([]string{"open one of these on your phone:"}, out...)
}

// shortcutHint points at the setup rather than printing the token itself:
// the TUI is the kind of thing that ends up in a screen share or a
// screenshot, and the token is the whole credential.
func shortcutHint(port int, configDir string) string {
	return fmt.Sprintf("iOS Shortcuts: POST to http://<this-host>:%d/upload  ·  token in %s",
		port, filepath.Join(configDir, uploadTokenFile))
}
