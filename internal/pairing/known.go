package pairing

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/saxill/beamdrop/internal/peer"
)

// DefaultKnownDir returns the XDG-respecting path to the known_peers directory.
// $XDG_CONFIG_HOME/beamdrop/known_peers, or $HOME/.config/beamdrop/known_peers.
func DefaultKnownDir() (string, error) {
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "beamdrop", "known_peers"), nil
}

type KnownPeers struct {
	dir   string
	store peer.Store
}

func NewKnownPeers(dir string, store peer.Store) (*KnownPeers, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("known peers dir: %w", err)
	}
	if err := store.Load(dir); err != nil {
		return nil, fmt.Errorf("known peers load: %w", err)
	}
	return &KnownPeers{dir: dir, store: store}, nil
}

func (k *KnownPeers) IsKnown(pubkey [32]byte) (peer.Peer, bool) {
	return k.store.Get(pubkey)
}

func (k *KnownPeers) Remember(p peer.Peer) error {
	k.store.Add(p)
	return k.store.Save(k.dir)
}

func (k *KnownPeers) All() []peer.Peer {
	return k.store.All()
}
