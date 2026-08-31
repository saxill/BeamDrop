package webui

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Tailscale will issue a real, publicly trusted Let's Encrypt certificate
// for a node's MagicDNS name. Using it instead of a self-signed one is
// worth the trouble because self-signed keeps costing more than it looks:
//
//   - Safari's interstitial has to be tapped through on every device, and
//     it hard-refuses some certificates with no way forward at all.
//   - A service worker needs a secure context, which a tapped-through
//     certificate does not reliably provide, so the page cannot install
//     properly as an app.
//   - iOS does not extend a manually accepted certificate to WebSocket
//     connections, so pairing can fail silently after the page loads.
//
// A real certificate removes all three at once.
//
// Everything here degrades to self-signed: no tailscale binary, HTTPS not
// enabled on the tailnet, not logged in, command fails — all just mean
// falling back.

const (
	// Asking for the name is a local query against tailscaled, so a short
	// bound is right: if it is not answering quickly it is not answering.
	statusTimeout = 5 * time.Second
	// Issuance is longer because the first one talks to Let's Encrypt.
	// After that tailscaled serves the cert from its own cache, so repeated
	// calls are cheap and do not re-hit the CA — which matters, because the
	// CA rate-limits duplicate certificates per week.
	issueTimeout = 60 * time.Second
)

// tailscaleRunner is the seam tests replace so they neither shell out nor
// touch the network.
type tailscaleRunner interface {
	CertDomain(ctx context.Context) (string, error)
	IssueCert(ctx context.Context, domain, certPath, keyPath string) error
}

var tailscaleCLI tailscaleRunner = cliRunner{}

type cliRunner struct{}

// CertDomain returns the name this node can get a certificate for.
// Tailscale leaves it empty when HTTPS is not enabled for the tailnet, so
// it doubles as the capability check.
func (cliRunner) CertDomain(ctx context.Context) (string, error) {
	out, err := exec.CommandContext(ctx, "tailscale", "status", "--json").Output()
	if err != nil {
		return "", err
	}
	var status struct {
		CertDomains []string `json:"CertDomains"`
	}
	if err := json.Unmarshal(out, &status); err != nil {
		return "", err
	}
	if len(status.CertDomains) == 0 {
		return "", fmt.Errorf("tailscale: HTTPS certificates are not enabled for this tailnet")
	}
	return strings.TrimSuffix(status.CertDomains[0], "."), nil
}

func (cliRunner) IssueCert(ctx context.Context, domain, certPath, keyPath string) error {
	cmd := exec.CommandContext(ctx, "tailscale", "cert",
		"--cert-file", certPath, "--key-file", keyPath, domain)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("tailscale cert: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// CertDomain reports the MagicDNS name this machine can serve a real
// certificate for, or "" if it cannot. The portal uses it to print the
// address worth opening on the phone: that name is the only one the
// certificate covers, so printing the IP instead would hand the user a URL
// that still shows a warning.
func CertDomain() string {
	ctx, cancel := context.WithTimeout(context.Background(), statusTimeout)
	defer cancel()
	domain, err := tailscaleCLI.CertDomain(ctx)
	if err != nil {
		return ""
	}
	return domain
}

func tailscaleCertPaths(dir string) (certPath, keyPath string) {
	// Kept apart from the self-signed pair so neither can be mistaken for
	// the other: the self-signed one is regenerated whenever this machine's
	// addresses change, which would be wrong for this one.
	return filepath.Join(dir, "tailscale-cert.pem"), filepath.Join(dir, "tailscale-key.pem")
}

// tailscaleTLS returns a Tailscale-issued certificate and the domain it
// covers. ok is false whenever anything is missing, which is the ordinary
// case on a machine that is not on a tailnet.
func tailscaleTLS(dir string) (cfg *tls.Config, domain string, ok bool) {
	if dir == "" {
		return nil, "", false
	}
	ctx, cancel := context.WithTimeout(context.Background(), issueTimeout)
	defer cancel()

	domain, err := tailscaleCLI.CertDomain(ctx)
	if err != nil || domain == "" {
		return nil, "", false
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, "", false
	}
	certPath, keyPath := tailscaleCertPaths(dir)
	// A failed issuance is not a reason to throw away a certificate that is
	// already here and still valid. tailscaled may be briefly down, the CA
	// may be rate-limiting, or this user may lack permission to ask for a
	// renewal on a machine where someone with permission fetched one
	// earlier. In all of those the right move is to keep serving what we
	// have until it actually expires.
	if err := tailscaleCLI.IssueCert(ctx, domain, certPath, keyPath); err != nil {
		if _, statErr := os.Stat(certPath); statErr != nil {
			return nil, "", false
		}
	}
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return nil, "", false
	}
	// An expired certificate on disk is worse than none: the phone reports
	// an error with no obvious remedy, where self-signed at least offers a
	// tap-through.
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return nil, "", false
	}
	now := time.Now()
	if now.Before(leaf.NotBefore) || now.After(leaf.NotAfter) {
		return nil, "", false
	}
	cert.Leaf = leaf
	return &tls.Config{Certificates: []tls.Certificate{cert}}, domain, true
}
