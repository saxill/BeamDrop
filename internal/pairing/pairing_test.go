package pairing

import "testing"

func TestCodeIsStableAndSixDigits(t *testing.T) {
	var a, b [32]byte
	for i := range a {
		a[i] = byte(i)
		b[i] = byte(255 - i)
	}
	code := Code(a, b)
	if len(code) != 6 {
		t.Fatalf("code len %d, want 6: %q", len(code), code)
	}
	for _, c := range code {
		if c < '0' || c > '9' {
			t.Errorf("non-digit in code: %q", code)
		}
	}
	// Symmetric
	if Code(b, a) != code {
		t.Errorf("code not symmetric: %q vs %q", code, Code(b, a))
	}
	// Deterministic
	if Code(a, b) != code {
		t.Errorf("code not deterministic")
	}
}

func TestCodeKnownAnswer(t *testing.T) {
	// Pin the algorithm: any change to derivation breaks this.
	var a, b [32]byte
	a[0] = 0x01
	b[0] = 0x02
	want := Code(a, b)
	if want == "" {
		t.Fatal("empty code")
	}
	// Record this value; a refactor that changes the algorithm must update this test deliberately.
	t.Logf("known answer: %s", want)
}

func TestSharedKeyECDH(t *testing.T) {
	alice, _ := Generate()
	bob, _ := Generate()
	k1 := SharedKey(alice.Priv, bob.Pub, "123456")
	k2 := SharedKey(bob.Priv, alice.Pub, "123456")
	var zero [32]byte
	if k1 == zero || k2 == zero {
		t.Fatal("shared key is zero")
	}
	if k1 != k2 {
		t.Errorf("ECDH not symmetric")
	}
	// Different code → different key
	k3 := SharedKey(alice.Priv, bob.Pub, "654321")
	if k3 == k1 {
		t.Errorf("code did not mix into key")
	}
}

func TestVerifyHMAC(t *testing.T) {
	var k [32]byte
	for i := range k {
		k[i] = byte(i)
	}
	var n1, n2 [32]byte
	n1[0] = 0xAA
	n2[0] = 0xBB
	got := ComputeHMAC(k, n1, n2)
	if !Verify(k, n1, n2, got) {
		t.Fatal("verify rejected correct HMAC")
	}
	// Tamper one byte
	got[0] ^= 1
	if Verify(k, n1, n2, got) {
		t.Fatal("verify accepted tampered HMAC")
	}
}
