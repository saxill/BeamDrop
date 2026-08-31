// Package push delivers Web Push notifications to phones that have
// subscribed, so a file arriving while the app is closed is still noticed.
//
// This is the one part of beamdrop that talks to the public internet. A
// push goes to the vendor's push service — Apple's for an iOS home-screen
// app — which then wakes the phone. There is no way around that: the phone
// is asleep and only its own vendor can wake it. What travels is encrypted
// end to end with keys the push service does not have, so the service
// learns that a message went to a device and how big it was, not what it
// said.
//
// Three preconditions, all of which are now met but none of which were
// before:
//
//   - the page must be served over a *publicly trusted* certificate. A
//     tapped-through self-signed one does not qualify;
//   - the page must be installed to the home screen (iOS 16.4+);
//   - the user must grant permission, which can only be asked for from a
//     tap.
package push

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	webpush "github.com/SherClockHolmes/webpush-go"
)

// Subscription is one device's registration.
type Subscription struct {
	ID       string    `json:"id"`
	Peer     string    `json:"peer"`
	PeerKey  string    `json:"peer_key"` // hex of the beamdrop pubkey that registered it
	Endpoint string    `json:"endpoint"`
	P256dh   string    `json:"p256dh"`
	Auth     string    `json:"auth"`
	Added    time.Time `json:"added"`
}

// Notification is what the phone shows.
type Notification struct {
	Title string `json:"title"`
	Body  string `json:"body"`
	Tag   string `json:"tag,omitempty"`
}

// Store holds the VAPID identity and the known subscriptions.
type Store struct {
	dir     string
	pub     string
	priv    string
	subject string

	mu   sync.Mutex
	subs map[string]Subscription
}

const (
	keysFile = "vapid.json"
	subsDir  = "subscriptions"
)

// DefaultSubject is the `sub` claim in the VAPID token: a real contact the
// push service can reach about this sender.
//
// Deliberately a bare address with no "mailto:" scheme. webpush-go prepends
// one itself for anything that is not an https URL, so passing
// "mailto:someone@example.com" here produces "mailto:mailto:someone@..."
// in the token and Apple answers 403 {"reason":"BadJwtToken"} — for every
// send, to every device, with nothing in the local logs to say why. The
// only symptom is a phone that stays quiet.
//
// It also has to be an address that could actually receive mail. An earlier
// version used beamdrop@localhost on the reasoning that a plainly fake
// contact beat an invented real-looking one; that is a defensible instinct
// and the wrong call here, because the push service checks.
// Anyone running their own copy should point this at an address they own.
// BEAMDROP_VAPID_SUBJECT does that without editing the source; the constant
// is only the fallback for the deployment this was written on.
const fallbackSubject = "admin@sahilchanna.co.in"

// DefaultSubject is the contact address the push service is given.
func DefaultSubject() string {
	if s := strings.TrimSpace(os.Getenv("BEAMDROP_VAPID_SUBJECT")); s != "" {
		return s
	}
	return fallbackSubject
}

type vapidKeys struct {
	Public  string `json:"public"`
	Private string `json:"private"`
}

// Open loads or creates the VAPID identity under dir and reads any
// subscriptions already there.
//
// The keypair must be stable: it is baked into every subscription a phone
// has already made, so regenerating it silently invalidates all of them and
// notifications simply stop, with the phone still believing it is
// subscribed.
func Open(dir, subject string) (*Store, error) {
	if dir == "" {
		return nil, errors.New("push: no config directory")
	}
	if err := os.MkdirAll(filepath.Join(dir, subsDir), 0o700); err != nil {
		return nil, err
	}
	if subject == "" {
		subject = DefaultSubject()
	}
	s := &Store{dir: dir, subject: subject, subs: map[string]Subscription{}}

	keyPath := filepath.Join(dir, keysFile)
	b, err := os.ReadFile(keyPath)
	switch {
	case err == nil:
		var k vapidKeys
		if err := json.Unmarshal(b, &k); err != nil {
			return nil, fmt.Errorf("push: %s: %w", keyPath, err)
		}
		s.priv, s.pub = k.Private, k.Public
	case errors.Is(err, os.ErrNotExist):
		priv, pub, gerr := webpush.GenerateVAPIDKeys()
		if gerr != nil {
			return nil, gerr
		}
		out, _ := json.MarshalIndent(vapidKeys{Public: pub, Private: priv}, "", "  ")
		if werr := os.WriteFile(keyPath, out, 0o600); werr != nil {
			return nil, werr
		}
		s.priv, s.pub = priv, pub
	default:
		return nil, err
	}

	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

// PublicKey is what the page passes to pushManager.subscribe.
func (s *Store) PublicKey() string { return s.pub }

func (s *Store) load() error {
	entries, err := os.ReadDir(filepath.Join(s.dir, subsDir))
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(s.dir, subsDir, e.Name()))
		if err != nil {
			continue
		}
		var sub Subscription
		if err := json.Unmarshal(b, &sub); err != nil || sub.Endpoint == "" {
			continue
		}
		s.subs[sub.ID] = sub
	}
	return nil
}

// id derives a stable identifier from the endpoint, so a device that
// re-subscribes replaces its old record instead of accumulating one per
// reconnect — a phone re-subscribes often.
func id(endpoint string) string {
	h := sha256Hex(endpoint)
	return h[:32]
}

// Add stores a subscription, replacing any earlier one for the same
// endpoint.
func (s *Store) Add(sub Subscription) error {
	if sub.Endpoint == "" || sub.P256dh == "" || sub.Auth == "" {
		return errors.New("push: incomplete subscription")
	}
	sub.ID = id(sub.Endpoint)
	if sub.Added.IsZero() {
		sub.Added = time.Now()
	}
	b, err := json.MarshalIndent(sub, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(s.dir, subsDir, sub.ID+".json")
	if err := os.WriteFile(path, b, 0o600); err != nil {
		return err
	}
	s.mu.Lock()
	s.subs[sub.ID] = sub
	s.mu.Unlock()
	return nil
}

// All returns the current subscriptions, newest first.
func (s *Store) All() []Subscription {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Subscription, 0, len(s.subs))
	for _, v := range s.subs {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Added.After(out[j].Added) })
	return out
}

func (s *Store) remove(subID string) {
	s.mu.Lock()
	delete(s.subs, subID)
	s.mu.Unlock()
	_ = os.Remove(filepath.Join(s.dir, subsDir, subID+".json"))
}

// Notify sends to every subscription except the one belonging to
// exceptPeerKey, which is the device that caused the event — telling a
// phone about the photo it just sent is noise, and it is the one device
// certain to already know.
//
// Errors are reported per subscription rather than aborting: one dead
// registration must not stop the others being told.
func (s *Store) Notify(n Notification, exceptPeerKey string) []error {
	payload, err := json.Marshal(n)
	if err != nil {
		return []error{err}
	}
	var errs []error
	for _, sub := range s.All() {
		if exceptPeerKey != "" && strings.EqualFold(sub.PeerKey, exceptPeerKey) {
			continue
		}
		if err := s.sendTo(sub, payload); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", sub.Peer, err))
		}
	}
	return errs
}

func (s *Store) sendTo(sub Subscription, payload []byte) error {
	resp, err := webpush.SendNotification(payload, &webpush.Subscription{
		Endpoint: sub.Endpoint,
		Keys:     webpush.Keys{P256dh: sub.P256dh, Auth: sub.Auth},
	}, &webpush.Options{
		Subscriber:      s.subject,
		VAPIDPublicKey:  s.pub,
		VAPIDPrivateKey: s.priv,
		TTL:             int((24 * time.Hour).Seconds()),
		// Urgency high: this is a person waiting on a file, not a digest.
		Urgency: webpush.UrgencyHigh,
	})
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	// 404/410 mean the push service has permanently dropped this
	// registration — the app was deleted, or permission revoked. Keeping it
	// would mean retrying a dead endpoint forever.
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone {
		s.remove(sub.ID)
		return fmt.Errorf("subscription gone (%d), forgotten", resp.StatusCode)
	}
	// 401/403 are not about this device. They mean the push service rejected
	// our VAPID identity, so every send fails the same way — which is worth
	// saying out loud, because the symptom is a phone that stays quiet and
	// there is nothing else to notice.
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return fmt.Errorf("push service rejected our identity (%d) — notifications will not arrive "+
			"for any device until the VAPID subject %q is one it accepts", resp.StatusCode, s.subject)
	}
	if resp.StatusCode >= 300 {
		return fmt.Errorf("push service returned %d", resp.StatusCode)
	}
	return nil
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}
