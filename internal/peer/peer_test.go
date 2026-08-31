package peer

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMemStoreAddGet(t *testing.T) {
	s := NewMemStore()
	var pk [32]byte
	copy(pk[:], "test-pubkey-1234567890abcdef")
	s.Add(Peer{Name: "iphone", PubKey: pk, LastIP: "100.64.0.2"})
	got, ok := s.Get(pk)
	if !ok {
		t.Fatal("not found")
	}
	if got.Name != "iphone" || got.LastIP != "100.64.0.2" {
		t.Errorf("got %+v", got)
	}
}

func TestMemStoreAll(t *testing.T) {
	s := NewMemStore()
	for i := 0; i < 3; i++ {
		var pk [32]byte
		pk[0] = byte(i)
		s.Add(Peer{Name: "p", PubKey: pk})
	}
	if got := len(s.All()); got != 3 {
		t.Errorf("got %d, want 3", got)
	}
}

func TestMemStoreSaveLoad(t *testing.T) {
	dir := t.TempDir()
	s := NewMemStore()
	var pk [32]byte
	copy(pk[:], "persist-test")
	s.Add(Peer{Name: "saved", PubKey: pk, LastIP: "10.0.0.1"})

	if err := s.Save(dir); err != nil {
		t.Fatal(err)
	}
	// file should be named after the hex pubkey
	files, _ := os.ReadDir(dir)
	if len(files) != 1 {
		t.Fatalf("expected 1 file, got %d", len(files))
	}
	if filepath.Ext(files[0].Name()) != ".peer" {
		t.Errorf("expected .peer extension, got %s", files[0].Name())
	}

	s2 := NewMemStore()
	if err := s2.Load(dir); err != nil {
		t.Fatal(err)
	}
	got, ok := s2.Get(pk)
	if !ok || got.Name != "saved" || got.LastIP != "10.0.0.1" {
		t.Errorf("loaded wrong: %+v ok=%v", got, ok)
	}
}
