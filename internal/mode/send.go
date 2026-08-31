package mode

import (
	"fmt"
	"net"
	"os"
	"time"

	"github.com/saxill/beamdrop/internal/config"
	"github.com/saxill/beamdrop/internal/engine"
	"github.com/saxill/beamdrop/internal/pairing"
	"github.com/saxill/beamdrop/internal/peer"
	"github.com/saxill/beamdrop/internal/transfer"
)

type SendOptions struct {
	Port        int
	Peer        string // host or IP of the portal; empty means loopback
	CodeTimeout time.Duration
}

func Send(path string, opts SendOptions) error {
	cfg := config.Defaults()
	if opts.Port == 0 {
		opts.Port = cfg.Port
	}
	if opts.CodeTimeout == 0 {
		opts.CodeTimeout = 30 * time.Second
	}
	// Open the file before dialling. A typo in the path should not make
	// someone confirm a pairing prompt for a transfer that cannot happen.
	s, err := transfer.NewSender(path)
	if err != nil {
		return err
	}
	defer s.Close()

	identity, known, err := localIdentity(cfg)
	if err != nil {
		return err
	}

	addr, how, err := findPortal(opts.Peer, opts.Port, known)
	if err != nil {
		return err
	}
	if how != "" {
		fmt.Printf("beamdrop: connecting to %s (%s)\n", addr, how)
	} else {
		fmt.Printf("beamdrop: connecting to %s\n", addr)
	}
	conn, err := net.DialTimeout("tcp", addr, 10*time.Second)
	if err != nil {
		return fmt.Errorf("send: dial %s: %w (is `beamdrop portal` running there?)", addr, err)
	}
	defer conn.Close()

	e, err := engine.New(engine.Config{
		Name:        hostName(),
		Conn:        conn,
		IsInitiator: true,
		Identity:    &identity,
		Known:       known,
		Confirmer: func(initPub, respPub [32]byte, peerName, code string, isKnown bool) bool {
			// The initiating side has nothing to decide — it already chose
			// to connect. Printing the code is what gives the portal's
			// prompt meaning: the human compares the two.
			if isKnown {
				fmt.Printf("beamdrop: %s (already paired)\n", peerName)
			} else {
				fmt.Printf("beamdrop: pairing with %s — code %s\n", peerName, code)
				fmt.Println("beamdrop: confirm the same code in the portal to continue")
			}
			return true
		},
	})
	if err != nil {
		return err
	}
	if err := e.SendFile(s, nil); err != nil {
		return err
	}
	fmt.Printf("beamdrop: sent %s (%d bytes)\n", s.Path, s.Size)
	return nil
}

// localIdentity loads this machine's persistent keypair and its known-peers
// store. Together they are what let a second `beamdrop send` skip the
// confirmation the first one needed.
func localIdentity(cfg config.Config) (pairing.KeyPair, *pairing.KnownPeers, error) {
	id, err := pairing.LoadOrCreateIdentity(cfg.ConfigDir)
	if err != nil {
		return pairing.KeyPair{}, nil, err
	}
	known, err := pairing.NewKnownPeers(cfg.KnownPeersDir, peer.NewMemStore())
	if err != nil {
		return pairing.KeyPair{}, nil, err
	}
	return id, known, nil
}

func hostName() string {
	h, _ := os.Hostname()
	return h
}
