package mode

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func inboxServer(t *testing.T) (*peerServer, string) {
	t.Helper()
	inbox := t.TempDir()
	return &peerServer{
		opts:     peerServerOptions{InboxDir: inbox, Logf: func(string, ...any) {}},
		messages: newMessageLog(),
	}, inbox
}

func TestInboxFileResolvesARealFile(t *testing.T) {
	s, inbox := inboxServer(t)
	want := filepath.Join(inbox, "photo.jpg")
	if err := os.WriteFile(want, []byte("bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := s.inboxFile("photo.jpg")
	if err != nil {
		t.Fatal(err)
	}
	// EvalSymlinks may canonicalise /tmp, so compare resolved forms.
	wantResolved, _ := filepath.EvalSymlinks(want)
	if got != wantResolved {
		t.Errorf("got %q, want %q", got, wantResolved)
	}
}

// The name arrives from a paired peer and is entirely under its control.
// filepath.Join *cleans* "../" rather than rejecting it, so joining a
// peer-supplied path would let a request walk out of the inbox.
func TestInboxFileRefusesEscapes(t *testing.T) {
	s, inbox := inboxServer(t)
	// Something worth stealing, one level up from the inbox.
	secret := filepath.Join(filepath.Dir(inbox), "secret.txt")
	if err := os.WriteFile(secret, []byte("private"), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{
		"../secret.txt",
		"../../secret.txt",
		"..",
		".",
		"/etc/passwd",
		"subdir/../../secret.txt",
		string(filepath.Separator),
		"",
	} {
		t.Run(name, func(t *testing.T) {
			got, err := s.inboxFile(name)
			if err == nil {
				t.Fatalf("%q was accepted and resolved to %q", name, got)
			}
			if strings.Contains(got, "secret") {
				t.Fatalf("%q reached the secret file", name)
			}
		})
	}
}

// A symlink planted in the inbox must not become a way to read elsewhere.
func TestInboxFileRefusesASymlinkOut(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privilege on Windows")
	}
	s, inbox := inboxServer(t)
	secret := filepath.Join(filepath.Dir(inbox), "secret.txt")
	if err := os.WriteFile(secret, []byte("private"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(inbox, "innocent.txt")
	if err := os.Symlink(secret, link); err != nil {
		t.Skipf("cannot create symlink: %v", err)
	}

	if got, err := s.inboxFile("innocent.txt"); err == nil {
		t.Errorf("a symlink out of the inbox was served: %q", got)
	}
}

func TestInboxFileRefusesADirectory(t *testing.T) {
	s, inbox := inboxServer(t)
	if err := os.Mkdir(filepath.Join(inbox, "adir"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := s.inboxFile("adir"); err == nil {
		t.Error("a directory was accepted as a file to send")
	}
}

func TestInboxFileRefusesSomethingMissing(t *testing.T) {
	s, _ := inboxServer(t)
	if _, err := s.inboxFile("never-existed.txt"); err == nil {
		t.Error("a missing file was accepted")
	}
}
