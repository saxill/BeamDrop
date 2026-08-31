package pairing

import (
	"os"
	"path/filepath"
	"testing"
)

// TestLoadOrCreateIdentityIsStableAcrossCalls is the property the whole TOFU
// story rests on: without it every connection looks like a new stranger and
// the known-peers store can never match.
func TestLoadOrCreateIdentityIsStableAcrossCalls(t *testing.T) {
	dir := t.TempDir()

	first, err := LoadOrCreateIdentity(dir)
	if err != nil {
		t.Fatal(err)
	}
	second, err := LoadOrCreateIdentity(dir)
	if err != nil {
		t.Fatal(err)
	}
	if first.Priv != second.Priv {
		t.Error("second call returned a different private key")
	}
	if first.Pub != second.Pub {
		t.Error("second call returned a different public key")
	}
	if first.Pub == ([32]byte{}) {
		t.Error("public key is all zeros")
	}
}

func TestLoadOrCreateIdentityWritesPrivateKeyRestrictively(t *testing.T) {
	dir := t.TempDir()
	if _, err := LoadOrCreateIdentity(dir); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(dir, IdentityFile))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("identity is mode %#o, want no group/other access", perm)
	}
}

// TestLoadOrCreateIdentityRejectsCorruptFile: silently regenerating would
// invalidate every peer that already remembered this machine, and they
// would report it as a key mismatch with nothing to explain it.
func TestLoadOrCreateIdentityRejectsCorruptFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, IdentityFile), []byte("not a key"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreateIdentity(dir); err == nil {
		t.Error("a corrupt identity file was accepted")
	}
}

// TestLoadedIdentityDerivesTheSameSharedKeyAsAFreshOne guards the re-clamp
// on load: an unclamped key derives a public key the peer cannot reproduce,
// and the failure would look like a bug in pairing rather than in storage.
func TestLoadedIdentityDerivesTheSameSharedKeyAsAFreshOne(t *testing.T) {
	dir := t.TempDir()
	stored, err := LoadOrCreateIdentity(dir)
	if err != nil {
		t.Fatal(err)
	}
	reloaded, err := LoadOrCreateIdentity(dir)
	if err != nil {
		t.Fatal(err)
	}
	other, err := Generate()
	if err != nil {
		t.Fatal(err)
	}

	code := Code(stored.Pub, other.Pub)
	mine := SharedKey(reloaded.Priv, other.Pub, code)
	theirs := SharedKey(other.Priv, stored.Pub, code)
	if mine != theirs {
		t.Error("a reloaded identity does not agree on the shared key")
	}
}
