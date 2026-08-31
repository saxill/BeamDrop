// Package peer holds the in-memory + on-disk registry of paired devices.
package peer

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type Peer struct {
	Name     string    `json:"name"`
	PubKey   [32]byte  `json:"pubkey"`
	LastIP   string    `json:"last_ip"`
	LastSeen time.Time `json:"last_seen"`
}

type Store interface {
	Add(p Peer)
	Get(pubkey [32]byte) (Peer, bool)
	All() []Peer
	Save(dir string) error
	Load(dir string) error
}

type MemStore struct {
	mu    sync.RWMutex
	peers map[[32]byte]Peer
}

func NewMemStore() *MemStore {
	return &MemStore{peers: map[[32]byte]Peer{}}
}

func (m *MemStore) Add(p Peer) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if p.LastSeen.IsZero() {
		p.LastSeen = time.Now()
	}
	m.peers[p.PubKey] = p
}

func (m *MemStore) Get(pk [32]byte) (Peer, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	p, ok := m.peers[pk]
	return p, ok
}

func (m *MemStore) All() []Peer {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Peer, 0, len(m.peers))
	for _, p := range m.peers {
		out = append(out, p)
	}
	return out
}

// Save writes one JSON file per peer, named <hex-pubkey>.peer.
func (m *MemStore) Save(dir string) error {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	for _, p := range m.peers {
		fname := filepath.Join(dir, hex.EncodeToString(p.PubKey[:])+".peer")
		b, err := json.MarshalIndent(p, "", "  ")
		if err != nil {
			return err
		}
		if err := os.WriteFile(fname, b, 0o600); err != nil {
			return err
		}
	}
	return nil
}

func (m *MemStore) Load(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, e := range entries {
		if filepath.Ext(e.Name()) != ".peer" {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return fmt.Errorf("peer %s: %w", e.Name(), err)
		}
		var p Peer
		if err := json.Unmarshal(b, &p); err != nil {
			return fmt.Errorf("peer %s: %w", e.Name(), err)
		}
		m.peers[p.PubKey] = p
	}
	return nil
}
