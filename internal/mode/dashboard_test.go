package mode

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/saxill/beamdrop/internal/pairing"
	"github.com/saxill/beamdrop/internal/peer"
	"github.com/saxill/beamdrop/internal/spool"
)

func dashServer(t *testing.T) (*peerServer, string) {
	t.Helper()
	known, err := pairing.NewKnownPeers(t.TempDir(), peer.NewMemStore())
	if err != nil {
		t.Fatal(err)
	}
	sp, err := spool.Open(filepath.Join(t.TempDir(), "spool"))
	if err != nil {
		t.Fatal(err)
	}
	inbox := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	srv, err := startPeerServer(ctx, peerServerOptions{
		ListenAddr:  "127.0.0.1:0",
		InboxDir:    inbox,
		Known:       known,
		Spool:       sp,
		UploadToken: testToken,
		Confirmer:   func(a, b [32]byte, n, c string, k bool) bool { return true },
		Logf:        func(string, ...any) {},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Close() })
	return srv, inbox
}

func get(t *testing.T, url string) (*http.Response, string) {
	t.Helper()
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Get(url)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, string(b)
}

func TestDashboardShowsInboxSpoolAndActivity(t *testing.T) {
	srv, inbox := dashServer(t)
	if err := os.WriteFile(filepath.Join(inbox, "IMG_0001.jpeg"), []byte("photo"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.opts.Spool.Add("some-laptop", "held.pdf", strings.NewReader("x")); err != nil {
		t.Fatal(err)
	}
	srv.activity.Logf("received something.txt (12 bytes)")

	resp, body := get(t, "http://"+srv.Addr().String()+"/status?token="+testToken)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	for _, want := range []string{"IMG_0001.jpeg", "held.pdf", "some-laptop", "received something.txt"} {
		if !strings.Contains(body, want) {
			t.Errorf("dashboard does not mention %q", want)
		}
	}
}

// TestDashboardNeedsTailnetAndToken: the page lists filenames and links
// that carry the token, so it is behind the same door as uploads.
func TestDashboardNeedsTailnetAndToken(t *testing.T) {
	srv, _ := dashServer(t)
	base := "http://" + srv.Addr().String()

	resp, body := get(t, base+"/status")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("no token = %d, want 401", resp.StatusCode)
	}
	// Unhelpful errors are their own bug; say where the token lives.
	if !strings.Contains(body, uploadTokenFile) {
		t.Errorf("401 body does not say where to find the token: %q", body)
	}

	resp, _ = get(t, base+"/status?token=wrong")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("wrong token = %d, want 401", resp.StatusCode)
	}
}

// TestDashboardNeverPrintsTheToken: a dashboard is the sort of thing left
// open on a second screen. Links must carry the credential, the page body
// must not display it.
func TestDashboardNeverPrintsTheToken(t *testing.T) {
	srv, inbox := dashServer(t)
	if err := os.WriteFile(filepath.Join(inbox, "a.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, body := get(t, "http://"+srv.Addr().String()+"/status?token="+testToken)

	// It appears inside href attributes; it must not appear as visible text.
	visible := body
	for {
		i := strings.Index(visible, "<a href=")
		if i < 0 {
			break
		}
		j := strings.Index(visible[i:], ">")
		if j < 0 {
			break
		}
		visible = visible[:i] + visible[i+j+1:]
	}
	if strings.Contains(visible, testToken) {
		t.Error("the token is rendered as visible text on the dashboard")
	}
}

func TestDashboardDownloadsFromInbox(t *testing.T) {
	srv, inbox := dashServer(t)
	want := "the actual file contents"
	if err := os.WriteFile(filepath.Join(inbox, "note.txt"), []byte(want), 0o644); err != nil {
		t.Fatal(err)
	}
	resp, body := get(t, "http://"+srv.Addr().String()+"/files?name=note.txt&token="+testToken)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if body != want {
		t.Errorf("body = %q, want %q", body, want)
	}
	if cd := resp.Header.Get("Content-Disposition"); !strings.Contains(cd, "note.txt") {
		t.Errorf("Content-Disposition = %q", cd)
	}
}

// TestDownloadCannotEscapeTheInbox: the name arrives in a URL, so it is
// input like any other.
func TestDownloadCannotEscapeTheInbox(t *testing.T) {
	srv, inbox := dashServer(t)
	outside := filepath.Join(filepath.Dir(inbox), "secret.txt")
	if err := os.WriteFile(outside, []byte("not yours"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"../secret.txt", "..%2Fsecret.txt", "/etc/hostname"} {
		resp, body := get(t, "http://"+srv.Addr().String()+"/files?name="+name+"&token="+testToken)
		if resp.StatusCode == http.StatusOK && strings.Contains(body, "not yours") {
			t.Errorf("%q escaped the inbox", name)
		}
	}
}

func TestActivityLogKeepsNewestAndBounds(t *testing.T) {
	a := newActivityLog(nil)
	for i := 0; i < activityMax+50; i++ {
		a.Logf("line %d", i)
	}
	recent := a.Recent(5)
	if len(recent) != 5 {
		t.Fatalf("Recent(5) = %d entries", len(recent))
	}
	if !strings.Contains(recent[0].Text, "line 349") {
		t.Errorf("newest entry = %q, want the last line written", recent[0].Text)
	}
	if got := len(a.Recent(1000)); got != activityMax {
		t.Errorf("log holds %d entries, want it bounded at %d", got, activityMax)
	}
}
