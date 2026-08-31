package mode

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/saxill/beamdrop/internal/frame"
)

func historyServer(t *testing.T, inbox string) *peerServer {
	t.Helper()
	return &peerServer{
		opts:     peerServerOptions{InboxDir: inbox, Logf: func(string, ...any) {}},
		messages: newMessageLog(),
	}
}

// Outbound is from the *asking* peer's point of view. Getting it backwards
// puts the phone's own photos on the laptop's side of the conversation,
// which reads as the laptop having sent you your own pictures.
func TestHistoryMarksTheAskersOwnMessagesAsTheirs(t *testing.T) {
	s := historyServer(t, t.TempDir())
	now := time.Now()
	s.messages.record(Message{At: now, Peer: "iPhone", Kind: MessageText, Text: "from the phone"})
	s.messages.record(Message{At: now.Add(time.Second), Peer: "laptop", Kind: MessageText, Text: "from another device"})
	s.messages.record(Message{At: now.Add(2 * time.Second), Peer: "iPhone", Outbound: true, Kind: MessageText, Text: "we sent this to the phone"})

	got := map[string]bool{}
	for _, e := range s.historyFor("iPhone") {
		got[e.Text] = e.Outbound
	}

	if !got["from the phone"] {
		t.Error("a message the phone sent is not marked as the phone's")
	}
	if got["from another device"] {
		t.Error("another device's message was attributed to the phone")
	}
	if got["we sent this to the phone"] {
		t.Error("a message sent *to* the phone was marked as sent *by* it")
	}
}

// Peer names come from a HELLO the peer chose, so casing is not guaranteed
// to match what was recorded.
func TestHistoryMatchesTheAskerCaseInsensitively(t *testing.T) {
	s := historyServer(t, t.TempDir())
	s.messages.record(Message{At: time.Now(), Peer: "iPhone", Kind: MessageText, Text: "hi"})
	entries := s.historyFor("IPHONE")
	if len(entries) != 1 {
		t.Fatalf("got %d entries", len(entries))
	}
	if !entries[0].Outbound {
		t.Error("case difference in the peer name lost the ownership of a message")
	}
}

// Files in the inbox are part of the conversation, and are what the user is
// most often looking for when they reopen the app.
func TestHistoryIncludesInboxFiles(t *testing.T) {
	inbox := t.TempDir()
	if err := os.WriteFile(filepath.Join(inbox, "photo.jpg"), []byte("12345"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := historyServer(t, inbox)

	var file *frame.HistoryEntry
	for i, e := range s.historyFor("iPhone") {
		if e.Kind == "file" {
			file = &s.historyFor("iPhone")[i]
			break
		}
	}
	if file == nil {
		t.Fatal("no file entry in the history")
	}
	if file.Name != "photo.jpg" {
		t.Errorf("Name = %q", file.Name)
	}
	if file.Size != 5 {
		t.Errorf("Size = %d, want 5", file.Size)
	}
}

// A phone on cellular should not be made to pull a wall of filenames before
// the page can draw anything, and the whole list has to fit in one frame.
func TestHistoryIsCapped(t *testing.T) {
	s := historyServer(t, t.TempDir())
	for i := 0; i < historyMax*3; i++ {
		s.messages.record(Message{
			At:   time.Now().Add(time.Duration(i) * time.Second),
			Kind: MessageText, Peer: "iPhone", Text: "filler",
		})
	}
	entries := s.historyFor("iPhone")
	if len(entries) > historyMax {
		t.Errorf("history has %d entries, cap is %d", len(entries), historyMax)
	}
	// And it must encode inside one frame.
	if _, err := frame.Encode(frame.HistoryType, frame.HistoryPayload{Entries: entries}); err != nil {
		t.Errorf("a full history does not encode: %v", err)
	}
}

// The oldest thing is not what you want when you reopen the app.
func TestHistoryKeepsTheMostRecent(t *testing.T) {
	s := historyServer(t, t.TempDir())
	base := time.Now().Add(-time.Hour)
	for i := 0; i < historyMax+10; i++ {
		s.messages.record(Message{
			At:   base.Add(time.Duration(i) * time.Second),
			Kind: MessageText, Peer: "iPhone", Text: string(rune('a' + i%26)),
		})
	}
	entries := s.historyFor("iPhone")
	if len(entries) == 0 {
		t.Fatal("no entries")
	}
	newest := entries[len(entries)-1].At
	oldest := entries[0].At
	if newest <= oldest {
		t.Error("entries are not oldest-first")
	}
	if want := base.Add(time.Duration(historyMax+9) * time.Second).Unix(); newest != want {
		t.Errorf("newest entry is %d, want the most recent message %d", newest, want)
	}
}

func TestHistoryRoundTripsThroughAFrame(t *testing.T) {
	in := []frame.HistoryEntry{
		{At: 1700000000, Kind: "text", Text: "hello ☃", Outbound: true, Peer: "iPhone"},
		{At: 1700000060, Kind: "file", Name: "a b/c.jpg", Size: 4096},
	}
	buf, err := frame.Encode(frame.HistoryType, frame.HistoryPayload{Entries: in})
	if err != nil {
		t.Fatal(err)
	}
	typ, payload, err := frame.Decode(buf)
	if err != nil {
		t.Fatal(err)
	}
	if typ != frame.HistoryType {
		t.Fatalf("type = 0x%02x", typ)
	}
	out, err := frame.DecodeHistory(payload)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != len(in) {
		t.Fatalf("got %d entries, want %d", len(out), len(in))
	}
	for i := range in {
		if out[i] != in[i] {
			t.Errorf("entry %d: got %+v, want %+v", i, out[i], in[i])
		}
	}
}

// An empty history is a normal answer from a fresh portal, not a fault.
func TestEmptyHistoryEncodesAsAnArray(t *testing.T) {
	buf, err := frame.Encode(frame.HistoryType, frame.HistoryPayload{Entries: nil})
	if err != nil {
		t.Fatal(err)
	}
	_, payload, err := frame.Decode(buf)
	if err != nil {
		t.Fatal(err)
	}
	// "null" would make the browser's for-of throw; it has to be "[]".
	if string(payload) != "[]" {
		t.Errorf("payload = %q, want []", payload)
	}
	out, err := frame.DecodeHistory(payload)
	if err != nil || len(out) != 0 {
		t.Errorf("decode: %v, %d entries", err, len(out))
	}
}
