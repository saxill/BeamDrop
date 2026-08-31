package webui

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// certLifetime is deliberately under the 398-day ceiling Safari enforces on
// server certificates; a longer-lived cert is rejected outright rather than
// offered as a warning the user can tap through.
const certLifetime = 365 * 24 * time.Hour

// localSANs enumerates every address a phone could plausibly reach this
// laptop on. Getting this wrong is not a warning the user can dismiss:
// Safari refuses a certificate whose SANs do not cover the host that was
// typed, so a cert naming only 127.0.0.1 makes the Tailscale address —
// the entire point of beamdrop working off-LAN — unreachable from iOS.
func localSANs() (ips []net.IP, names []string) {
	seen := map[string]bool{}
	add := func(ip net.IP) {
		// An unspecified address (0.0.0.0, ::) names no host and matches
		// nothing; multicast addresses are not connection endpoints.
		if ip == nil || ip.IsUnspecified() || ip.IsMulticast() {
			return
		}
		if s := ip.String(); !seen[s] {
			seen[s] = true
			ips = append(ips, ip)
		}
	}
	add(net.IPv4(127, 0, 0, 1))
	add(net.IPv6loopback)
	// Every interface address, which is what picks up the Tailscale 100.x
	// address without beamdrop needing to know Tailscale exists.
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if ipnet, ok := a.(*net.IPNet); ok {
			add(ipnet.IP)
		}
	}

	names = append(names, "localhost")
	if h, err := os.Hostname(); err == nil && h != "" {
		names = append(names, h)
		if !strings.Contains(h, ".") {
			// mDNS name, so `beamdrop.local` works on the same LAN.
			names = append(names, h+".local")
		}
	}
	return ips, names
}

// serverTLS builds the config the phone-facing server runs on. It holds two
// certificates and picks between them per connection:
//
//   - the Tailscale-issued one, when the phone connected by MagicDNS name.
//     This is publicly trusted, so there is no interstitial at all;
//   - the self-signed one otherwise, which is what an IP address gets.
//
// Both are needed. The real certificate covers only the DNS name, so
// serving it to someone who typed the tailnet IP would turn today's
// tap-through warning into a name-mismatch error — a regression for the
// address the README has been telling people to use. And the self-signed
// one can never carry the DNS name credibly. Which one applies is decided
// by SNI, which a browser omits entirely for an IP address, so the two
// cases separate cleanly.
func serverTLS(certDir string) (*tls.Config, error) {
	self, err := selfSigned(certDir)
	if err != nil {
		return nil, err
	}
	tsCfg, domain, ok := tailscaleTLS(certDir)
	if !ok {
		return self, nil
	}
	selfCert, tsCert := &self.Certificates[0], &tsCfg.Certificates[0]
	return &tls.Config{
		// Certificates is deliberately left unset. Go only consults
		// GetCertificate when Certificates is empty *or* the client sent
		// SNI — so filling both in would silently bypass this callback for
		// exactly the no-SNI case it exists to handle.
		GetCertificate: func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
			if strings.EqualFold(strings.TrimSuffix(hello.ServerName, "."), domain) {
				return tsCert, nil
			}
			return selfCert, nil
		},
	}, nil
}

// selfSigned returns a self-signed TLS config for the web UI.
//
// When certDir is set the keypair is persisted there and reused across
// restarts. That matters more than it looks: iOS remembers a manually
// accepted self-signed certificate by identity, so a freshly generated one
// on every launch means re-tapping through the interstitial every single
// time the portal restarts. When certDir is empty the cert is ephemeral
// (used by tests).
func selfSigned(certDir string) (*tls.Config, error) {
	if certDir != "" {
		if cert, ok := loadPersistedCert(certDir); ok {
			return &tls.Config{Certificates: []tls.Certificate{cert}}, nil
		}
	}
	certPEM, keyPEM, err := generateCert()
	if err != nil {
		return nil, err
	}
	if certDir != "" {
		// Best-effort: an unwritable config dir should degrade to an
		// ephemeral cert, not stop the portal from starting.
		_ = persistCert(certDir, certPEM, keyPEM)
	}
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, err
	}
	return &tls.Config{Certificates: []tls.Certificate{cert}}, nil
}

func generateCert() (certPEM, keyPEM []byte, err error) {
	ips, names := localSANs()
	return generateCertFor(ips, names)
}

func generateCertFor(ips []net.IP, names []string) (certPEM, keyPEM []byte, err error) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, err
	}
	tmpl := x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "beamdrop"},
		NotBefore:             time.Now().Add(-time.Hour), // tolerate clock skew against the phone
		NotAfter:              time.Now().Add(certLifetime),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		// Not a CA. A server certificate carrying CA:TRUE is rejected by
		// Safari outright — not the "not private, visit anyway"
		// interstitial you can tap through, but a hard stop with no way
		// forward. Self-signed is fine; self-signed *and* claiming to be a
		// certificate authority is not.
		IsCA:        false,
		IPAddresses: ips,
		DNSNames:    names,
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &priv.PublicKey, priv)
	if err != nil {
		return nil, nil, err
	}
	keyBytes, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		return nil, nil, err
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyBytes})
	return certPEM, keyPEM, nil
}

func certPaths(dir string) (certPath, keyPath string) {
	return filepath.Join(dir, "webui-cert.pem"), filepath.Join(dir, "webui-key.pem")
}

// loadPersistedCert returns a previously saved keypair, but only if it is
// still usable: expired certs, and certs that predate a change in this
// machine's addresses (a new Tailscale IP, a different network), have to be
// regenerated or the phone gets a cert error with no way to recover short
// of deleting files by hand.
func loadPersistedCert(dir string) (tls.Certificate, bool) {
	certPath, keyPath := certPaths(dir)
	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		return tls.Certificate{}, false
	}
	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		return tls.Certificate{}, false
	}
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return tls.Certificate{}, false
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return tls.Certificate{}, false
	}
	now := time.Now()
	if now.Before(leaf.NotBefore) || now.After(leaf.NotAfter) {
		return tls.Certificate{}, false
	}
	if !coversCurrentAddrs(leaf) {
		return tls.Certificate{}, false
	}
	// Regenerate anything issued while this code marked its own server
	// certificate as a CA. Without this the fix would only reach people who
	// happened to delete the file by hand, and the phone would keep
	// refusing to connect with no indication why.
	if leaf.IsCA {
		return tls.Certificate{}, false
	}
	cert.Leaf = leaf
	return cert, true
}

func coversCurrentAddrs(leaf *x509.Certificate) bool {
	have := map[string]bool{}
	for _, ip := range leaf.IPAddresses {
		have[ip.String()] = true
	}
	ips, _ := localSANs()
	for _, ip := range ips {
		if !have[ip.String()] {
			return false
		}
	}
	return true
}

func persistCert(dir string, certPEM, keyPEM []byte) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	certPath, keyPath := certPaths(dir)
	if err := os.WriteFile(certPath, certPEM, 0o644); err != nil {
		return err
	}
	// 0600: this is a private key, and the directory may be shared.
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		return fmt.Errorf("webui: write key: %w", err)
	}
	return nil
}
