package webui

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"os"
	"testing"
	"time"
)

// fakeTailscale stands in for the CLI. It issues a certificate for the
// domain the same way tailscaled would hand one over: as a PEM pair on
// disk. Nothing here touches the network or a real Let's Encrypt account.
type fakeTailscale struct {
	domain    string
	domainErr error
	issueErr  error
	issued    int
}

func (f *fakeTailscale) CertDomain(context.Context) (string, error) {
	if f.domainErr != nil {
		return "", f.domainErr
	}
	return f.domain, nil
}

func (f *fakeTailscale) IssueCert(_ context.Context, domain, certPath, keyPath string) error {
	if f.issueErr != nil {
		return f.issueErr
	}
	f.issued++
	certPEM, keyPEM, err := generateCertFor(nil, []string{domain})
	if err != nil {
		return err
	}
	if err := os.WriteFile(certPath, certPEM, 0o644); err != nil {
		return err
	}
	return os.WriteFile(keyPath, keyPEM, 0o600)
}

func withFake(t *testing.T, f tailscaleRunner) {
	t.Helper()
	prev := tailscaleCLI
	tailscaleCLI = f
	t.Cleanup(func() { tailscaleCLI = prev })
}

// servedCert completes a handshake with the given SNI and reports the
// certificate the server chose.
func servedCert(t *testing.T, cfg *tls.Config, serverName string) *tls.Certificate {
	t.Helper()
	client, server := net.Pipe()
	t.Cleanup(func() { client.Close(); server.Close() })

	go func() {
		s := tls.Server(server, cfg)
		s.SetDeadline(time.Now().Add(5 * time.Second))
		_ = s.Handshake()
	}()

	c := tls.Client(client, &tls.Config{
		InsecureSkipVerify: true,
		ServerName:         serverName,
	})
	c.SetDeadline(time.Now().Add(5 * time.Second))
	if err := c.Handshake(); err != nil {
		t.Fatalf("handshake with SNI %q: %v", serverName, err)
	}
	peers := c.ConnectionState().PeerCertificates
	if len(peers) == 0 {
		t.Fatalf("no certificate served for SNI %q", serverName)
	}
	return &tls.Certificate{Leaf: peers[0]}
}

// The whole point of holding two certificates: the MagicDNS name gets the
// publicly trusted one, and an IP address — which sends no SNI at all —
// still gets the self-signed cert that actually covers IPs. Serving the
// Tailscale cert to an IP visitor would replace a tap-through warning with
// a hard name mismatch.
func TestServerTLSPicksCertBySNI(t *testing.T) {
	const domain = "laptop.tail1234.ts.net"
	withFake(t, &fakeTailscale{domain: domain})

	cfg, err := serverTLS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	byName := servedCert(t, cfg, domain)
	if got := byName.Leaf.DNSNames; len(got) != 1 || got[0] != domain {
		t.Errorf("SNI %q got a cert for %v, want the Tailscale one", domain, got)
	}

	// Empty SNI is exactly what a browser sends when the user types an IP,
	// and a non-matching name is what a LAN hostname sends. Both must get
	// the self-signed cert, which is the only one covering IPs at all.
	for _, sni := range []string{"", "localhost", "laptop.local"} {
		got := servedCert(t, cfg, sni)
		if len(got.Leaf.IPAddresses) == 0 {
			t.Errorf("SNI %q was served the Tailscale cert, which covers no IP addresses", sni)
		}
		if len(got.Leaf.DNSNames) == 1 && got.Leaf.DNSNames[0] == domain {
			t.Errorf("SNI %q was served the Tailscale cert; an IP visitor would get a name mismatch", sni)
		}
	}
}

// A machine that is not on a tailnet is the ordinary case, not an error.
func TestServerTLSFallsBackWhenTailscaleUnavailable(t *testing.T) {
	for _, tc := range []struct {
		name string
		fake *fakeTailscale
	}{
		{"no tailscale", &fakeTailscale{domainErr: errors.New("executable file not found")}},
		{"https not enabled", &fakeTailscale{domainErr: errors.New("no cert domains")}},
		{"issuance fails", &fakeTailscale{domain: "x.ts.net", issueErr: errors.New("rate limited")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withFake(t, tc.fake)
			cfg, err := serverTLS(t.TempDir())
			if err != nil {
				t.Fatalf("serverTLS should degrade, not fail: %v", err)
			}
			cert := servedCert(t, cfg, "")
			if cert.Leaf.Subject.CommonName != "beamdrop" {
				t.Errorf("got CN %q, want the self-signed fallback",
					cert.Leaf.Subject.CommonName)
			}
		})
	}
}

// Renewal can fail on a machine where someone with more privilege fetched
// the certificate earlier — which is exactly the Pi, where `tailscale cert`
// needs root. Throwing away a still-valid certificate in that case would
// drop the phone back to the interstitial for no reason.
func TestTailscaleTLSKeepsValidCertWhenIssuanceFails(t *testing.T) {
	const domain = "pi.tail1234.ts.net"
	dir := t.TempDir()

	// Someone privileged fetched it once.
	withFake(t, &fakeTailscale{domain: domain})
	if _, _, ok := tailscaleTLS(dir); !ok {
		t.Fatal("first issuance should have succeeded")
	}

	// Now every renewal is denied.
	withFake(t, &fakeTailscale{domain: domain, issueErr: errors.New("Access denied: cert access denied")})
	cfg, got, ok := tailscaleTLS(dir)
	if !ok {
		t.Fatal("a valid on-disk certificate was discarded because renewal failed")
	}
	if got != domain {
		t.Errorf("domain = %q, want %q", got, domain)
	}
	leaf := cfg.Certificates[0].Leaf
	if leaf == nil || len(leaf.DNSNames) != 1 || leaf.DNSNames[0] != domain {
		t.Errorf("served the wrong certificate: %v", leaf)
	}
}

// An expired cert left on disk is worse than none: the phone reports an
// error with no way through, where self-signed at least offers a tap.
func TestTailscaleTLSRejectsExpiredCert(t *testing.T) {
	dir := t.TempDir()
	withFake(t, &fakeTailscale{domain: "x.ts.net"})

	certPath, keyPath := tailscaleCertPaths(dir)
	certPEM, keyPEM, err := expiredCertFor("x.ts.net")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(certPath, certPEM, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	// IssueCert in the fake overwrites with a fresh cert, so to exercise the
	// expiry check specifically, make issuance a no-op that leaves the stale
	// files in place — which is what tailscaled does when it believes its
	// cached copy is still good.
	tailscaleCLI = &noopIssuer{domain: "x.ts.net"}

	if _, _, ok := tailscaleTLS(dir); ok {
		t.Error("an expired certificate was accepted")
	}
}

func expiredCertFor(domain string) (certPEM, keyPEM []byte, err error) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	tmpl := x509.Certificate{
		SerialNumber:          big.NewInt(3),
		Subject:               pkix.Name{CommonName: domain},
		NotBefore:             time.Now().Add(-48 * time.Hour),
		NotAfter:              time.Now().Add(-24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              []string{domain},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &priv.PublicKey, priv)
	if err != nil {
		return nil, nil, err
	}
	keyBytes, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		return nil, nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyBytes}), nil
}

type noopIssuer struct{ domain string }

func (n *noopIssuer) CertDomain(context.Context) (string, error) { return n.domain, nil }
func (n *noopIssuer) IssueCert(context.Context, string, string, string) error {
	return nil
}
