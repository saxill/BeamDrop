package webui

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"testing"
	"time"
)

// TestSelfSignedCoversEveryLocalAddress is the test that stands between
// beamdrop and "works on localhost, unreachable from the phone": Safari
// will not offer a tap-through for a certificate whose SANs miss the
// address that was typed, so the Tailscale address has to be in there.
func TestSelfSignedCoversEveryLocalAddress(t *testing.T) {
	cfg, err := selfSigned("")
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(cfg.Certificates[0].Certificate[0])
	if err != nil {
		t.Fatal(err)
	}

	for _, ip := range interfaceIPs(t) {
		if err := leaf.VerifyHostname(ip.String()); err != nil {
			t.Errorf("certificate does not cover local address %s: %v", ip, err)
		}
	}
	if err := leaf.VerifyHostname("127.0.0.1"); err != nil {
		t.Errorf("certificate does not cover 127.0.0.1: %v", err)
	}
	if err := leaf.VerifyHostname("localhost"); err != nil {
		t.Errorf("certificate does not cover localhost: %v", err)
	}

	// Safari rejects (rather than warns about) server certificates valid
	// for more than 398 days.
	if d := leaf.NotAfter.Sub(leaf.NotBefore); d > 398*24*time.Hour {
		t.Errorf("certificate lifetime %v exceeds the 398-day limit Safari enforces", d)
	}
	for _, ip := range leaf.IPAddresses {
		if ip.IsUnspecified() {
			t.Errorf("certificate carries the unspecified address %s as a SAN, which names no host", ip)
		}
	}
}

// TestSelfSignedReusesPersistedCert covers the restart case: a new identity
// every launch means re-accepting the interstitial on the phone every
// launch.
func TestSelfSignedReusesPersistedCert(t *testing.T) {
	dir := t.TempDir()

	first, err := selfSigned(dir)
	if err != nil {
		t.Fatal(err)
	}
	second, err := selfSigned(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first.Certificates[0].Certificate[0], second.Certificates[0].Certificate[0]) {
		t.Error("second call generated a new certificate instead of reusing the persisted one")
	}

	_, keyPath := certPaths(dir)
	info, err := os.Stat(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("private key is mode %#o, want no group/other access", perm)
	}
}

// TestSelfSignedRegeneratesWhenAddressesChange: a persisted cert that no
// longer covers this machine's addresses is worse than no cert at all,
// because the failure surfaces on the phone as an error with no obvious
// remedy.
func TestSelfSignedRegeneratesWhenAddressesChange(t *testing.T) {
	dir := t.TempDir()

	stale, staleKey, err := generateCertFor([]net.IP{net.IPv4(192, 0, 2, 1)}, []string{"stale.invalid"})
	if err != nil {
		t.Fatal(err)
	}
	if err := persistCert(dir, stale, staleKey); err != nil {
		t.Fatal(err)
	}

	cfg, err := selfSigned(dir)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(cfg.Certificates[0].Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := leaf.VerifyHostname("127.0.0.1"); err != nil {
		t.Errorf("stale certificate was reused despite not covering 127.0.0.1: %v", err)
	}
}

func interfaceIPs(t *testing.T) []net.IP {
	t.Helper()
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		t.Fatal(err)
	}
	var out []net.IP
	for _, a := range addrs {
		ipnet, ok := a.(*net.IPNet)
		if !ok || ipnet.IP.IsUnspecified() || ipnet.IP.IsMulticast() {
			continue
		}
		// Skip IPv6 link-local: VerifyHostname compares the textual form,
		// and a zone-scoped address never round-trips cleanly.
		if ipnet.IP.To4() == nil && ipnet.IP.IsLinkLocalUnicast() {
			continue
		}
		out = append(out, ipnet.IP)
	}
	return out
}

// TestSelfSignedIsNotACertificateAuthority: Safari rejects a server
// certificate carrying CA:TRUE outright — no interstitial, no way through —
// so this is the difference between the phone connecting and not.
func TestSelfSignedIsNotACertificateAuthority(t *testing.T) {
	cfg, err := selfSigned("")
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(cfg.Certificates[0].Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	if leaf.IsCA {
		t.Error("the server certificate claims to be a CA; Safari will refuse it")
	}
	if leaf.KeyUsage&x509.KeyUsageCertSign != 0 {
		t.Error("the server certificate carries KeyUsageCertSign")
	}
	if !leaf.BasicConstraintsValid {
		t.Error("basic constraints are absent, so CA:FALSE is not actually asserted")
	}
}

// TestPersistedCACertIsReplaced: the broken certificates are already on
// disk on every machine that ran the previous build, so the fix has to
// evict them rather than only apply to fresh installs.
func TestPersistedCACertIsReplaced(t *testing.T) {
	dir := t.TempDir()

	// Write a cert shaped like the bad ones: valid, covering this machine,
	// but a CA.
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ips, names := localSANs()
	tmpl := x509.Certificate{
		SerialNumber:          big.NewInt(2),
		Subject:               pkix.Name{CommonName: "beamdrop"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		IPAddresses:           ips,
		DNSNames:              names,
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatal(err)
	}
	keyBytes, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	if err := persistCert(dir,
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyBytes})); err != nil {
		t.Fatal(err)
	}

	cfg, err := selfSigned(dir)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(cfg.Certificates[0].Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	if leaf.IsCA {
		t.Error("the persisted CA certificate was reused; the phone would still be refused")
	}
}
