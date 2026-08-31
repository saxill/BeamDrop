// Package pairing handles the 6-digit code, ECDH, and HMAC verification
// for beamdrop's trust-on-first-use (TOFU) peer authentication.
package pairing

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"fmt"

	"golang.org/x/crypto/blake2b"
	"golang.org/x/crypto/curve25519"
)

type KeyPair struct {
	Priv [32]byte
	Pub  [32]byte
}

// Generate returns a fresh curve25519 keypair.
func Generate() (KeyPair, error) {
	var kp KeyPair
	if _, err := rand.Read(kp.Priv[:]); err != nil {
		return kp, fmt.Errorf("pairing: rand: %w", err)
	}
	// Clamp per RFC 7748
	kp.Priv[0] &= 248
	kp.Priv[31] &= 127
	kp.Priv[31] |= 64
	kp.Pub = pubFromPriv(kp.Priv)
	return kp, nil
}

func pubFromPriv(priv [32]byte) [32]byte {
	var pub [32]byte
	curve25519.ScalarBaseMult(&pub, &priv)
	return pub
}

// Code returns the 6-digit pairing code, derived from the two public keys.
// Both sides of a pairing must display the same code, so the two public keys
// are sorted lexicographically and then hashed:
// BLAKE2b-256(min(initPub, respPub) || max(initPub, respPub)) mod 1_000_000,
// zero-padded.
func Code(initPub, respPub [32]byte) string {
	lo, hi := initPub[:], respPub[:]
	if bytes.Compare(hi, lo) < 0 {
		lo, hi = hi, lo
	}
	h, _ := blake2b.New256(nil)
	h.Write(lo)
	h.Write(hi)
	sum := h.Sum(nil)
	// Take first 8 bytes as little-endian uint64, then mod.
	n := binary.LittleEndian.Uint64(sum[:8])
	return fmt.Sprintf("%06d", n%1_000_000)
}

// SharedKey derives the 32-byte shared secret: BLAKE2b-256(ECDH || code).
func SharedKey(myPriv, theirPub [32]byte, code string) [32]byte {
	var ecdh [32]byte
	curve25519.ScalarMult(&ecdh, &myPriv, &theirPub)
	h, _ := blake2b.New256(nil)
	h.Write(ecdh[:])
	h.Write([]byte(code))
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

// ComputeHMAC returns HMAC-SHA256(k, initNonce || respNonce).
func ComputeHMAC(k, initNonce, respNonce [32]byte) [32]byte {
	h := hmac.New(sha256.New, k[:])
	h.Write(initNonce[:])
	h.Write(respNonce[:])
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

// Verify checks a received HMAC in constant time.
func Verify(k, initNonce, respNonce, got [32]byte) bool {
	want := ComputeHMAC(k, initNonce, respNonce)
	return subtle.ConstantTimeCompare(want[:], got[:]) == 1
}
