package transfer

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/saxill/beamdrop/internal/frame"
)

func TestSenderSmallFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "small.bin")
	data := []byte("hello beamdrop")
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := NewSender(p)
	if err != nil {
		t.Fatal(err)
	}
	if s.Size != uint64(len(data)) {
		t.Errorf("size: got %d, want %d", s.Size, len(data))
	}
	want := sha256.Sum256(data)
	if s.SHA256 != want {
		t.Errorf("sha256 mismatch")
	}
	offer, err := s.Offer()
	if err != nil {
		t.Fatal(err)
	}
	if offer.Name != "small.bin" {
		t.Errorf("name: %q", offer.Name)
	}
	if offer.SHA256 != want {
		t.Errorf("offer sha256 mismatch")
	}
	// Stream chunks
	var got []byte
	for {
		c, more, err := s.NextChunk(64 * 1024)
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, c.Data...)
		if !more {
			break
		}
	}
	if !bytes.Equal(got, data) {
		t.Errorf("streamed bytes mismatch")
	}
}

func TestSenderLargeFileChunks(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "big.bin")
	// 200KB file with random data → tests multiple chunks
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 200*1024)
	if _, err := io.ReadFull(rand.Reader, buf); err != nil {
		t.Fatal(err)
	}
	f.Write(buf)
	f.Close()

	s, err := NewSender(p)
	if err != nil {
		t.Fatal(err)
	}
	chunks := 0
	for {
		_, more, err := s.NextChunk(64 * 1024)
		if err != nil {
			t.Fatal(err)
		}
		chunks++
		if !more {
			break
		}
	}
	if chunks < 3 {
		t.Errorf("expected ≥3 chunks, got %d", chunks)
	}
}

func TestSenderHandleAckDoesNotPanic(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "x")
	os.WriteFile(p, []byte("x"), 0o644)
	s, _ := NewSender(p)
	s.HandleAck(frame.AckPayload{ID: s.ID, Offset: 1})
}

// TestNewSenderNamedOffersTheGivenName: a relay stores payloads under an
// opaque spool id, so the name on the wire cannot come from the path.
func TestNewSenderNamedOffersTheGivenName(t *testing.T) {
	dir := t.TempDir()
	stored := filepath.Join(dir, "1754392847-000001.data")
	if err := os.WriteFile(stored, []byte("photo bytes"), 0o600); err != nil {
		t.Fatal(err)
	}

	s, err := NewSenderNamed(stored, "IMG_0001.jpeg")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	offer, err := s.Offer()
	if err != nil {
		t.Fatal(err)
	}
	if offer.Name != "IMG_0001.jpeg" {
		t.Errorf("offered name = %q, want %q", offer.Name, "IMG_0001.jpeg")
	}
	if offer.Size != 11 {
		t.Errorf("size = %d, want 11", offer.Size)
	}
}

// A name is peer-visible metadata; a relay must not be able to pass a
// traversal through by naming a spooled item cleverly.
func TestNewSenderNamedSanitisesTheName(t *testing.T) {
	dir := t.TempDir()
	stored := filepath.Join(dir, "x.data")
	if err := os.WriteFile(stored, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := NewSenderNamed(stored, "../../etc/passwd")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	offer, err := s.Offer()
	if err != nil {
		t.Fatal(err)
	}
	if offer.Name != "passwd" {
		t.Errorf("offered name = %q, want %q", offer.Name, "passwd")
	}
}

// NewSender must keep behaving exactly as before.
func TestNewSenderStillUsesTheBaseName(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "report.pdf")
	if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := NewSender(p)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	offer, _ := s.Offer()
	if offer.Name != "report.pdf" {
		t.Errorf("offered name = %q, want report.pdf", offer.Name)
	}
}
