// Package spool holds files that arrived for a peer who was not reachable
// at the time, so an always-on relay can deliver them later.
//
// This is what makes a Pi (or any machine that stays up) useful in the
// middle: the phone hands a file to something that is always there, and
// that thing takes responsibility for getting it to a laptop which may be
// shut. Without it, sending only works when both ends happen to be awake at
// the same moment.
//
// Durability matters more than speed here. An item the relay accepted is an
// item the sender believes is delivered, so it has to survive a power cut
// on the relay itself.
package spool

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Kind says what a spooled item is. A text message goes through the same
// crash-safe queue as a file so that "send it when the laptop wakes up"
// means the same thing for both — a message that only survived while the
// relay happened to stay running would be a worse promise than not
// accepting it at all.
type Kind string

const (
	KindFile Kind = ""     // the zero value: everything written before Kind existed
	KindText Kind = "text" // payload is the message body, not a file
)

// Item is one file or message waiting to be delivered.
type Item struct {
	ID string `json:"id"`
	// To is the destination peer's name, as it appears in the known-peers
	// store. A name rather than an address because addresses change — the
	// whole point is that this outlives the peer being offline.
	To   string `json:"to"`
	Name string `json:"name"` // the file's name, already sanitised
	// Kind distinguishes a message from a file. Empty means file, so
	// everything spooled before this field existed still loads correctly.
	Kind       Kind      `json:"kind,omitempty"`
	Size       int64     `json:"size"`
	ReceivedAt time.Time `json:"received_at"`
	Attempts   int       `json:"attempts"`
	LastError  string    `json:"last_error,omitempty"`
	LastTry    time.Time `json:"last_try,omitempty"`

	dir string // set on load; not serialised
}

// Path is where this item's bytes live.
func (i Item) Path() string { return filepath.Join(i.dir, i.ID+dataExt) }

const (
	dataExt = ".data"
	metaExt = ".json"
	tmpExt  = ".partial"
)

// Spool is a directory of pending items.
type Spool struct {
	dir string
	mu  sync.Mutex
	seq uint64
}

func Open(dir string) (*Spool, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("spool: %w", err)
	}
	s := &Spool{dir: dir}
	s.sweepPartials()
	return s, nil
}

func (s *Spool) Dir() string { return s.dir }

// sweepPartials removes leftovers from a crash mid-write. They are
// identifiable precisely because the two-step commit below never leaves one
// behind on a successful add.
func (s *Spool) sweepPartials() {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), tmpExt) {
			_ = os.Remove(filepath.Join(s.dir, e.Name()))
		}
	}
	// A .data with no .json is also a partial: Add writes the metadata last
	// precisely so that its presence is the commit point.
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), dataExt) {
			continue
		}
		id := strings.TrimSuffix(e.Name(), dataExt)
		if _, err := os.Stat(filepath.Join(s.dir, id+metaExt)); os.IsNotExist(err) {
			_ = os.Remove(filepath.Join(s.dir, e.Name()))
		}
	}
}

// Add streams r into the spool for delivery to peer `to`.
//
// The metadata file is written last and is the commit point: an item is
// only visible to Pending once its sidecar exists, so a crash part-way
// through leaves nothing that looks deliverable.
func (s *Spool) Add(to, name string, r io.Reader) (Item, error) {
	return s.add(to, name, KindFile, r)
}

// AddText queues a message. It shares the file path deliberately: the
// durability, the retry loop and the expiry sweep are all things a message
// needs just as much, and duplicating them for a second queue would mean
// two chances to get crash-safety wrong instead of one.
func (s *Spool) AddText(to, body string) (Item, error) {
	return s.add(to, "message", KindText, strings.NewReader(body))
}

// Text reads back a KindText item's body.
func (s *Spool) Text(i Item) (string, error) {
	if i.Kind != KindText {
		return "", fmt.Errorf("spool: item %s is not a message", i.ID)
	}
	b, err := os.ReadFile(i.Path())
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func (s *Spool) add(to, name string, kind Kind, r io.Reader) (Item, error) {
	s.mu.Lock()
	s.seq++
	id := fmt.Sprintf("%d-%06d", time.Now().UnixNano(), s.seq)
	s.mu.Unlock()

	dataPath := filepath.Join(s.dir, id+dataExt)
	tmpPath := dataPath + tmpExt
	f, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return Item{}, fmt.Errorf("spool: create: %w", err)
	}
	n, err := io.Copy(f, r)
	if err == nil {
		// The relay told the sender this file was accepted, so it has to
		// still be here after a power cut.
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(tmpPath)
		return Item{}, fmt.Errorf("spool: write: %w", err)
	}
	if err := os.Rename(tmpPath, dataPath); err != nil {
		_ = os.Remove(tmpPath)
		return Item{}, fmt.Errorf("spool: commit data: %w", err)
	}

	item := Item{
		ID:         id,
		To:         to,
		Name:       filepath.Base(name),
		Kind:       kind,
		Size:       n,
		ReceivedAt: time.Now(),
		dir:        s.dir,
	}
	if err := s.writeMeta(item); err != nil {
		_ = os.Remove(dataPath)
		return Item{}, err
	}
	return item, nil
}

func (s *Spool) writeMeta(i Item) error {
	b, err := json.MarshalIndent(i, "", "  ")
	if err != nil {
		return err
	}
	metaPath := filepath.Join(s.dir, i.ID+metaExt)
	tmpPath := metaPath + tmpExt
	if err := os.WriteFile(tmpPath, b, 0o600); err != nil {
		return fmt.Errorf("spool: write meta: %w", err)
	}
	if err := os.Rename(tmpPath, metaPath); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("spool: commit meta: %w", err)
	}
	return nil
}

// Pending lists items awaiting delivery, oldest first so a backlog drains
// in the order it arrived.
func (s *Spool) Pending() ([]Item, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, err
	}
	var out []Item
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), metaExt) {
			continue
		}
		b, err := os.ReadFile(filepath.Join(s.dir, e.Name()))
		if err != nil {
			continue
		}
		var i Item
		if err := json.Unmarshal(b, &i); err != nil {
			continue // unreadable sidecar; leave it rather than delete data
		}
		i.dir = s.dir
		if _, err := os.Stat(i.Path()); err != nil {
			continue // metadata without payload; nothing to send
		}
		out = append(out, i)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].ReceivedAt.Before(out[b].ReceivedAt) })
	return out, nil
}

// PendingFor lists items destined for one peer.
func (s *Spool) PendingFor(to string) ([]Item, error) {
	all, err := s.Pending()
	if err != nil {
		return nil, err
	}
	var out []Item
	for _, i := range all {
		if i.To == to {
			out = append(out, i)
		}
	}
	return out, nil
}

// Done removes an item. Called only after the destination confirmed the
// file's hash — dropping it any earlier would lose the only copy.
func (s *Spool) Done(i Item) error {
	if err := os.Remove(filepath.Join(s.dir, i.ID+metaExt)); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.Remove(filepath.Join(s.dir, i.ID+dataExt)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// Failed records a delivery attempt that did not work, so the forwarder can
// back off and a human can see why.
func (s *Spool) Failed(i Item, cause error) error {
	i.Attempts++
	i.LastTry = time.Now()
	if cause != nil {
		msg := cause.Error()
		if len(msg) > 200 {
			msg = msg[:200]
		}
		i.LastError = msg
	}
	i.dir = s.dir
	return s.writeMeta(i)
}

// DefaultMaxAge is how long an undeliverable file is kept before it is
// dropped. A relay is often a Pi with a small card, and a peer that has not
// appeared in a month is not coming back for this file — but a month is
// also comfortably longer than any holiday, so nothing is lost that anyone
// was still expecting.
const DefaultMaxAge = 30 * 24 * time.Hour

// Expire removes items older than maxAge and reports what it dropped.
//
// Silence here would be the wrong behaviour: the sender was told these
// files were accepted, so their disappearance has to be visible somewhere
// rather than just freeing up space.
func (s *Spool) Expire(maxAge time.Duration) ([]Item, error) {
	if maxAge <= 0 {
		return nil, nil
	}
	items, err := s.Pending()
	if err != nil {
		return nil, err
	}
	cutoff := time.Now().Add(-maxAge)
	var dropped []Item
	for _, i := range items {
		if i.ReceivedAt.After(cutoff) {
			continue
		}
		if err := s.Done(i); err != nil {
			continue
		}
		dropped = append(dropped, i)
	}
	return dropped, nil
}

// Bytes reports how much disk the pending backlog is using.
func (s *Spool) Bytes() (int64, error) {
	items, err := s.Pending()
	if err != nil {
		return 0, err
	}
	var total int64
	for _, i := range items {
		total += i.Size
	}
	return total, nil
}

// Clear drops everything, for when a backlog is known to be stale.
func (s *Spool) Clear() (int, error) {
	items, err := s.Pending()
	if err != nil {
		return 0, err
	}
	n := 0
	for _, i := range items {
		if err := s.Done(i); err == nil {
			n++
		}
	}
	return n, nil
}
