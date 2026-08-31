package spool

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func openTemp(t *testing.T) *Spool {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "spool"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestAddThenPendingRoundTrips(t *testing.T) {
	s := openTemp(t)
	body := bytes.Repeat([]byte("photo"), 1000)

	item, err := s.Add("laptop", "IMG_0001.jpeg", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if item.Size != int64(len(body)) {
		t.Errorf("Size = %d, want %d", item.Size, len(body))
	}

	pending, err := s.Pending()
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 {
		t.Fatalf("Pending() = %d items, want 1", len(pending))
	}
	got := pending[0]
	if got.To != "laptop" || got.Name != "IMG_0001.jpeg" {
		t.Errorf("item = %+v", got)
	}
	onDisk, err := os.ReadFile(got.Path())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(onDisk, body) {
		t.Errorf("payload differs: %d bytes on disk, %d written", len(onDisk), len(body))
	}
}

// TestAddSanitisesTheName: the name reaches here from an HTTP header, so it
// is attacker-controlled exactly like FILE_OFFER.Name.
func TestAddSanitisesTheName(t *testing.T) {
	s := openTemp(t)
	for _, name := range []string{"../escape.bin", "/etc/passwd", "a/b/c.txt"} {
		item, err := s.Add("peer", name, strings.NewReader("x"))
		if err != nil {
			t.Fatal(err)
		}
		if strings.ContainsAny(item.Name, "/\\") || item.Name == ".." {
			t.Errorf("Add(%q) stored name %q", name, item.Name)
		}
	}
}

func TestPendingForFiltersByDestination(t *testing.T) {
	s := openTemp(t)
	for _, to := range []string{"laptop", "laptop", "phone"} {
		if _, err := s.Add(to, "f.bin", strings.NewReader("x")); err != nil {
			t.Fatal(err)
		}
	}
	laptop, err := s.PendingFor("laptop")
	if err != nil {
		t.Fatal(err)
	}
	if len(laptop) != 2 {
		t.Errorf("PendingFor(laptop) = %d, want 2", len(laptop))
	}
	none, err := s.PendingFor("nobody")
	if err != nil {
		t.Fatal(err)
	}
	if len(none) != 0 {
		t.Errorf("PendingFor(nobody) = %d, want 0", len(none))
	}
}

func TestPendingIsOldestFirst(t *testing.T) {
	// A backlog should drain in the order it arrived, not in whatever order
	// the filesystem hands back.
	s := openTemp(t)
	var ids []string
	for i := 0; i < 5; i++ {
		item, err := s.Add("laptop", "f.bin", strings.NewReader("x"))
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, item.ID)
	}
	pending, err := s.Pending()
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 5 {
		t.Fatalf("Pending() = %d, want 5", len(pending))
	}
	for i, want := range ids {
		if pending[i].ID != want {
			t.Errorf("position %d = %s, want %s", i, pending[i].ID, want)
		}
	}
}

func TestDoneRemovesBothFiles(t *testing.T) {
	s := openTemp(t)
	item, err := s.Add("laptop", "f.bin", strings.NewReader("hello"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Done(item); err != nil {
		t.Fatal(err)
	}
	pending, _ := s.Pending()
	if len(pending) != 0 {
		t.Errorf("Pending() = %d after Done, want 0", len(pending))
	}
	if _, err := os.Stat(item.Path()); !os.IsNotExist(err) {
		t.Error("payload survived Done")
	}
	if _, err := os.Stat(filepath.Join(s.Dir(), item.ID+metaExt)); !os.IsNotExist(err) {
		t.Error("metadata survived Done")
	}
}

func TestFailedRecordsAttemptsAndSurvivesReopen(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "spool")
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	item, err := s.Add("laptop", "f.bin", strings.NewReader("x"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Failed(item, errors.New("connection refused")); err != nil {
		t.Fatal(err)
	}

	// A relay restarts; the backlog and its history have to still be there.
	reopened, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := reopened.Pending()
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 {
		t.Fatalf("Pending() after reopen = %d, want 1", len(pending))
	}
	if pending[0].Attempts != 1 {
		t.Errorf("Attempts = %d, want 1", pending[0].Attempts)
	}
	if !strings.Contains(pending[0].LastError, "connection refused") {
		t.Errorf("LastError = %q", pending[0].LastError)
	}
}

// TestCrashMidWriteLeavesNothingDeliverable is the durability claim: the
// relay tells the sender a file was accepted, so a half-written item must
// never look like a complete one.
func TestCrashMidWriteLeavesNothingDeliverable(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "spool")
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	good, err := s.Add("laptop", "good.bin", strings.NewReader("real"))
	if err != nil {
		t.Fatal(err)
	}

	// Simulate the two ways a crash can leave things: a .partial from an
	// interrupted copy, and a .data whose sidecar was never written.
	if err := os.WriteFile(filepath.Join(dir, "999-000001.data.partial"), []byte("junk"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "999-000002.data"), []byte("orphan"), 0o600); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := reopened.Pending()
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].ID != good.ID {
		t.Fatalf("Pending() = %+v, want only the committed item", pending)
	}
	for _, leftover := range []string{"999-000001.data.partial", "999-000002.data"} {
		if _, err := os.Stat(filepath.Join(dir, leftover)); !os.IsNotExist(err) {
			t.Errorf("%s was not swept", leftover)
		}
	}
}

func TestSpoolFilesAreNotWorldReadable(t *testing.T) {
	// A relay holds other people's files; the directory may be on a shared
	// box.
	s := openTemp(t)
	item, err := s.Add("laptop", "private.bin", strings.NewReader("secret"))
	if err != nil {
		t.Fatal(err)
	}
	di, err := os.Stat(s.Dir())
	if err != nil {
		t.Fatal(err)
	}
	if perm := di.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("spool dir is %#o, want no group/other access", perm)
	}
	fi, err := os.Stat(item.Path())
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("spooled payload is %#o, want no group/other access", perm)
	}
}

func TestExpireDropsOnlyOldItems(t *testing.T) {
	s := openTemp(t)
	old, err := s.Add("laptop", "old.bin", strings.NewReader("x"))
	if err != nil {
		t.Fatal(err)
	}
	// Backdate it by rewriting the sidecar the same way Failed does.
	old.ReceivedAt = time.Now().Add(-40 * 24 * time.Hour)
	old.dir = s.Dir()
	if err := s.writeMeta(old); err != nil {
		t.Fatal(err)
	}
	fresh, err := s.Add("laptop", "fresh.bin", strings.NewReader("y"))
	if err != nil {
		t.Fatal(err)
	}

	dropped, err := s.Expire(30 * 24 * time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(dropped) != 1 || dropped[0].Name != "old.bin" {
		t.Fatalf("dropped = %+v, want just old.bin", dropped)
	}
	pending, _ := s.Pending()
	if len(pending) != 1 || pending[0].ID != fresh.ID {
		t.Errorf("pending = %+v, want just the fresh item", pending)
	}
	if _, err := os.Stat(old.Path()); !os.IsNotExist(err) {
		t.Error("expired payload was left on disk")
	}
}

func TestExpireIsANoOpWithoutAMaxAge(t *testing.T) {
	s := openTemp(t)
	if _, err := s.Add("laptop", "f.bin", strings.NewReader("x")); err != nil {
		t.Fatal(err)
	}
	dropped, err := s.Expire(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(dropped) != 0 {
		t.Errorf("Expire(0) dropped %d items; zero must mean 'keep everything'", len(dropped))
	}
	if pending, _ := s.Pending(); len(pending) != 1 {
		t.Error("Expire(0) removed a file")
	}
}

func TestBytesAndClear(t *testing.T) {
	s := openTemp(t)
	for i := 0; i < 3; i++ {
		if _, err := s.Add("laptop", "f.bin", strings.NewReader("0123456789")); err != nil {
			t.Fatal(err)
		}
	}
	total, err := s.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	if total != 30 {
		t.Errorf("Bytes() = %d, want 30", total)
	}
	n, err := s.Clear()
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Errorf("Clear() removed %d, want 3", n)
	}
	if pending, _ := s.Pending(); len(pending) != 0 {
		t.Errorf("%d items survived Clear", len(pending))
	}
}
