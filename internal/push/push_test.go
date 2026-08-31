package push

import (
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func openStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// The VAPID keypair is baked into every subscription a phone has already
// made. Regenerating it on restart silently invalidates all of them:
// notifications just stop, with the phone still believing it is subscribed.
func TestVapidKeyIsStableAcrossRestarts(t *testing.T) {
	dir := t.TempDir()
	first, err := Open(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	second, err := Open(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	if first.PublicKey() == "" {
		t.Fatal("no public key generated")
	}
	if first.PublicKey() != second.PublicKey() {
		t.Error("the VAPID key changed on restart; every existing subscription would go silent")
	}
}

func TestVapidPrivateKeyIsNotWorldReadable(t *testing.T) {
	dir := t.TempDir()
	if _, err := Open(dir, ""); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(dir, keysFile))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("vapid.json is %o; the signing key must not be group or world readable", perm)
	}
}

// A phone re-subscribes constantly. One record per reconnect would mean
// every notification sent N times to the same device.
func TestResubscribingReplacesRatherThanAccumulates(t *testing.T) {
	s := openStore(t)
	for i := 0; i < 5; i++ {
		if err := s.Add(Subscription{
			Peer: "iPhone", PeerKey: "aa",
			Endpoint: "https://push.example/abc", P256dh: "p", Auth: "a",
		}); err != nil {
			t.Fatal(err)
		}
	}
	if n := len(s.All()); n != 1 {
		t.Errorf("got %d subscriptions for one endpoint, want 1", n)
	}
}

func TestIncompleteSubscriptionIsRefused(t *testing.T) {
	s := openStore(t)
	for _, sub := range []Subscription{
		{Endpoint: "", P256dh: "p", Auth: "a"},
		{Endpoint: "https://push.example/x", P256dh: "", Auth: "a"},
		{Endpoint: "https://push.example/x", P256dh: "p", Auth: ""},
	} {
		if err := s.Add(sub); err == nil {
			t.Errorf("accepted an incomplete subscription: %+v", sub)
		}
	}
	if n := len(s.All()); n != 0 {
		t.Errorf("stored %d incomplete subscriptions", n)
	}
}

func TestSubscriptionsSurviveRestart(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Add(Subscription{
		Peer: "iPhone", PeerKey: "beef",
		Endpoint: "https://push.example/abc", P256dh: "p", Auth: "a",
	}); err != nil {
		t.Fatal(err)
	}
	again, err := Open(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	subs := again.All()
	if len(subs) != 1 {
		t.Fatalf("got %d subscriptions after restart, want 1", len(subs))
	}
	if subs[0].Peer != "iPhone" || subs[0].PeerKey != "beef" {
		t.Errorf("subscription came back wrong: %+v", subs[0])
	}
}

// pushServer stands in for the vendor's push service.
func pushServer(t *testing.T, status int) (*httptest.Server, *[]string) {
	t.Helper()
	var mu sync.Mutex
	var hit []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hit = append(hit, r.URL.Path)
		mu.Unlock()
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	return srv, &hit
}

// Telling a phone about the photo it just sent is noise, and it is the one
// device certain to already know.
func TestNotifySkipsTheDeviceThatCausedIt(t *testing.T) {
	srv, hit := pushServer(t, http.StatusCreated)
	s := openStore(t)

	if err := s.Add(Subscription{
		Peer: "iPhone", PeerKey: "aaaa",
		Endpoint: srv.URL + "/sender", P256dh: validP256dh, Auth: validAuth,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Add(Subscription{
		Peer: "iPad", PeerKey: "bbbb",
		Endpoint: srv.URL + "/other", P256dh: validP256dh, Auth: validAuth,
	}); err != nil {
		t.Fatal(err)
	}

	s.Notify(Notification{Title: "t", Body: "b"}, "aaaa")

	got := *hit
	for _, p := range got {
		if p == "/sender" {
			t.Error("notified the device that sent the file")
		}
	}
	found := false
	for _, p := range got {
		if p == "/other" {
			found = true
		}
	}
	if !found {
		t.Errorf("the other device was not notified; hits were %v", got)
	}
}

// 404/410 mean the registration is permanently dead — app deleted, or
// permission revoked. Keeping it means retrying a dead endpoint forever.
func TestGoneSubscriptionIsForgotten(t *testing.T) {
	srv, _ := pushServer(t, http.StatusGone)
	s := openStore(t)
	if err := s.Add(Subscription{
		Peer: "iPhone", PeerKey: "aaaa",
		Endpoint: srv.URL + "/dead", P256dh: validP256dh, Auth: validAuth,
	}); err != nil {
		t.Fatal(err)
	}

	if errs := s.Notify(Notification{Title: "t"}, ""); len(errs) == 0 {
		t.Error("a gone subscription reported no error")
	}
	if n := len(s.All()); n != 0 {
		t.Errorf("a permanently dead subscription is still stored (%d)", n)
	}
	// And it must be gone from disk too, or a restart brings it back.
	again, err := Open(s.dir, "")
	if err != nil {
		t.Fatal(err)
	}
	if n := len(again.All()); n != 0 {
		t.Errorf("the dead subscription came back after restart (%d)", n)
	}
}

// A transient failure is not a reason to forget a device that is fine.
func TestTemporaryFailureKeepsTheSubscription(t *testing.T) {
	srv, _ := pushServer(t, http.StatusInternalServerError)
	s := openStore(t)
	if err := s.Add(Subscription{
		Peer: "iPhone", Endpoint: srv.URL + "/flaky", P256dh: validP256dh, Auth: validAuth,
	}); err != nil {
		t.Fatal(err)
	}
	if errs := s.Notify(Notification{Title: "t"}, ""); len(errs) != 1 {
		t.Errorf("got %d errors, want 1", len(errs))
	}
	if n := len(s.All()); n != 1 {
		t.Errorf("a subscription was dropped after a temporary 500 (%d left)", n)
	}
}

// One dead registration must not stop the others being told.
func TestOneBadSubscriptionDoesNotStopTheRest(t *testing.T) {
	good, hit := pushServer(t, http.StatusCreated)
	s := openStore(t)
	if err := s.Add(Subscription{
		Peer: "broken", Endpoint: "https://127.0.0.1:1/nope", P256dh: validP256dh, Auth: validAuth,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Add(Subscription{
		Peer: "fine", Endpoint: good.URL + "/ok", P256dh: validP256dh, Auth: validAuth,
	}); err != nil {
		t.Fatal(err)
	}
	s.Notify(Notification{Title: "t"}, "")
	if len(*hit) == 0 {
		t.Error("the working device was never reached because another one failed")
	}
}

func TestNotificationEncodesWhatTheServiceWorkerReads(t *testing.T) {
	b, err := json.Marshal(Notification{Title: "laptop sent a file", Body: "photo.jpg", Tag: "file"})
	if err != nil {
		t.Fatal(err)
	}
	var back map[string]string
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	for k, want := range map[string]string{"title": "laptop sent a file", "body": "photo.jpg", "tag": "file"} {
		if back[k] != want {
			t.Errorf("%s = %q, want %q", k, back[k], want)
		}
	}
}

// A real browser subscription's keys, generated rather than hardcoded: the
// webpush library derives an ECDH shared secret from p256dh, so an invented
// value fails before any HTTP request is made and a delivery test would
// prove nothing. (An earlier version of this file learned that the hard
// way — every "was it delivered" assertion passed vacuously.)
var validP256dh, validAuth = browserKeys()

func browserKeys() (string, string) {
	priv, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		panic(err)
	}
	auth := make([]byte, 16) // the spec's auth secret length
	if _, err := rand.Read(auth); err != nil {
		panic(err)
	}
	enc := base64.RawURLEncoding
	return enc.EncodeToString(priv.PublicKey().Bytes()), enc.EncodeToString(auth)
}

// A 403 means the push service rejected our VAPID identity, so every send
// fails the same way. That is a configuration problem, not a dead device:
// the subscription must survive, and the message must say so — the only
// other symptom is a phone that stays quiet.
func TestRejectedIdentityIsReportedClearlyAndKeepsSubscriptions(t *testing.T) {
	for _, code := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		srv, _ := pushServer(t, code)
		s := openStore(t)
		if err := s.Add(Subscription{
			Peer: "iPhone", Endpoint: srv.URL + "/x", P256dh: validP256dh, Auth: validAuth,
		}); err != nil {
			t.Fatal(err)
		}
		errs := s.Notify(Notification{Title: "t"}, "")
		if len(errs) != 1 {
			t.Fatalf("%d: got %d errors, want 1", code, len(errs))
		}
		msg := errs[0].Error()
		if !strings.Contains(msg, "will not arrive") || !strings.Contains(msg, s.subject) {
			t.Errorf("%d: unhelpful message %q — it must name the cause and the subject", code, msg)
		}
		if n := len(s.All()); n != 1 {
			t.Errorf("%d: dropped a subscription over our own misconfiguration (%d left)", code, n)
		}
	}
}

// webpush-go prepends "mailto:" to any subject that is not an https URL.
// Passing one that already has the scheme yields "mailto:mailto:..." in the
// JWT, and Apple answers 403 BadJwtToken for every send to every device —
// discovered only by asking Apple for the response body, because nothing
// local says more than "403".
func TestDefaultSubjectIsNotDoublePrefixed(t *testing.T) {
	if strings.HasPrefix(DefaultSubject(), "mailto:") {
		t.Errorf("DefaultSubject() = %q; the library adds mailto: itself, so this becomes mailto:mailto:", DefaultSubject())
	}
}

// It also has to be a contact something could actually reach.
func TestDefaultSubjectIsAUsableContact(t *testing.T) {
	if strings.HasPrefix(DefaultSubject(), "https://") {
		return // an https URI is the other form RFC 8292 allows
	}
	if !strings.Contains(DefaultSubject(), "@") || strings.Contains(DefaultSubject(), "localhost") {
		t.Errorf("DefaultSubject() = %q is not an address anyone could write to", DefaultSubject())
	}
	domain := DefaultSubject()[strings.Index(DefaultSubject(), "@")+1:]
	if !strings.Contains(domain, ".") {
		t.Errorf("DefaultSubject() = %q has no real domain", DefaultSubject())
	}
}

// A fork should not have to edit the source to stop sending someone else's
// address to Apple.
func TestDefaultSubjectPrefersTheEnvironment(t *testing.T) {
	t.Setenv("BEAMDROP_VAPID_SUBJECT", "me@example.org")
	if got := DefaultSubject(); got != "me@example.org" {
		t.Errorf("DefaultSubject() = %q, want the environment's value", got)
	}
	t.Setenv("BEAMDROP_VAPID_SUBJECT", "   ")
	if got := DefaultSubject(); got != fallbackSubject {
		t.Errorf("DefaultSubject() = %q; blank should fall back, not be sent as-is", got)
	}
}
