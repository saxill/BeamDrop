package pairing

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/saxill/beamdrop/internal/peer"
)

func TestDefaultKnownDirRespectsXDG(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/tmp/xdg-test")
	dir, err := DefaultKnownDir()
	if err != nil {
		t.Fatal(err)
	}
	if dir != "/tmp/xdg-test/beamdrop/known_peers" {
		t.Errorf("got %s", dir)
	}
}

func TestKnownPeersRememberAndCheck(t *testing.T) {
	dir := t.TempDir()
	kp, err := NewKnownPeers(dir, peer.NewMemStore())
	if err != nil {
		t.Fatal(err)
	}
	var pk [32]byte
	copy(pk[:], "known-peer-test")
	p := peer.Peer{Name: "iphone", PubKey: pk, LastIP: "100.64.0.2"}
	if err := kp.Remember(p); err != nil {
		t.Fatal(err)
	}
	got, ok := kp.IsKnown(pk)
	if !ok || got.Name != "iphone" {
		t.Errorf("got %+v ok=%v", got, ok)
	}
	// File on disk
	files, _ := os.ReadDir(dir)
	if len(files) != 1 {
		t.Errorf("expected 1 peer file, got %d", len(files))
	}
	if filepath.Ext(files[0].Name()) != ".peer" {
		t.Errorf("expected .peer ext, got %s", files[0].Name())
	}
}
