package engine

import "sync"

// Registry tracks one Engine per connected peer, keyed by the peer's
// pairing public key. This is the "pool engines" architecture: each
// peer connection gets its own Engine instance (Engine.Serve blocks on
// that connection's inline ReadFrame loop), so multi-peer support means
// multiple registry entries, never one Engine shared across peers.
type Registry struct {
	mu      sync.Mutex
	engines map[[32]byte]*Engine
}

func NewRegistry() *Registry {
	return &Registry{engines: map[[32]byte]*Engine{}}
}

func (r *Registry) Add(e *Engine) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.engines[e.PeerPubKey()] = e
}

func (r *Registry) Remove(pub [32]byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.engines, pub)
}

func (r *Registry) Get(pub [32]byte) (*Engine, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.engines[pub]
	return e, ok
}

func (r *Registry) All() []*Engine {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*Engine, 0, len(r.engines))
	for _, e := range r.engines {
		out = append(out, e)
	}
	return out
}
