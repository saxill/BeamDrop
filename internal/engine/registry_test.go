package engine

import (
	"testing"

	"github.com/saxill/beamdrop/internal/pairing"
)

func TestRegistryAddGetRemoveAll(t *testing.T) {
	r := NewRegistry()
	e1 := &Engine{peerKeys: pairing.KeyPair{Pub: [32]byte{1}}}
	e2 := &Engine{peerKeys: pairing.KeyPair{Pub: [32]byte{2}}}
	r.Add(e1)
	r.Add(e2)
	if got, ok := r.Get([32]byte{1}); !ok || got != e1 {
		t.Errorf("Get(1) = %v, %v", got, ok)
	}
	if len(r.All()) != 2 {
		t.Errorf("All() len = %d, want 2", len(r.All()))
	}
	r.Remove([32]byte{1})
	if _, ok := r.Get([32]byte{1}); ok {
		t.Error("Get(1) after Remove: still present")
	}
	if len(r.All()) != 1 {
		t.Errorf("All() after Remove len = %d, want 1", len(r.All()))
	}
}
