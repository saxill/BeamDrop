package pairing

import (
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// IdentityFile is the name of the keypair file inside the config dir.
const IdentityFile = "identity.key"

// LoadOrCreateIdentity returns this machine's long-lived curve25519
// keypair, generating and saving one the first time.
//
// Trust-on-first-use needs a "first use" to mean something. With a fresh
// keypair per connection — which is what Generate() alone gives you — the
// peer on the other end sees an unrecognised stranger every single time,
// so the known-peers store can never match and the user re-confirms a
// 6-digit code on every send, every page load, every reconnect. A stable
// key is also what lets the *other* side notice that the machine it is
// talking to has changed.
func LoadOrCreateIdentity(dir string) (KeyPair, error) {
	path := filepath.Join(dir, IdentityFile)
	if kp, err := loadIdentity(path); err == nil {
		return kp, nil
	} else if !os.IsNotExist(err) {
		// A corrupt or unreadable identity is reported rather than silently
		// replaced: overwriting it would invalidate every peer that has
		// already remembered this machine, and they would surface that as a
		// key mismatch with no explanation.
		return KeyPair{}, fmt.Errorf("pairing: read %s: %w", path, err)
	}

	kp, err := Generate()
	if err != nil {
		return KeyPair{}, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return KeyPair{}, fmt.Errorf("pairing: identity dir: %w", err)
	}
	// 0600 — this is the private half of the machine's identity.
	if err := os.WriteFile(path, []byte(hex.EncodeToString(kp.Priv[:])+"\n"), 0o600); err != nil {
		return KeyPair{}, fmt.Errorf("pairing: write %s: %w", path, err)
	}
	return kp, nil
}

func loadIdentity(path string) (KeyPair, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return KeyPair{}, err
	}
	raw, err := hex.DecodeString(strings.TrimSpace(string(b)))
	if err != nil {
		return KeyPair{}, fmt.Errorf("not hex: %w", err)
	}
	if len(raw) != 32 {
		return KeyPair{}, fmt.Errorf("want 32 bytes, got %d", len(raw))
	}
	var kp KeyPair
	copy(kp.Priv[:], raw)
	// Re-clamp rather than trusting the file: a private key that was not
	// clamped per RFC 7748 would derive a public key the peer cannot
	// reproduce, and the failure would look like a pairing bug.
	kp.Priv[0] &= 248
	kp.Priv[31] &= 127
	kp.Priv[31] |= 64
	kp.Pub = pubFromPriv(kp.Priv)
	return kp, nil
}
