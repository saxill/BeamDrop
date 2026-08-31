package transfer

import (
	"bytes"
	"crypto/sha256"
	"os"
	"path/filepath"
	"testing"

	"github.com/saxill/beamdrop/internal/frame"
)

func TestReceiverRoundTrip(t *testing.T) {
	dir := t.TempDir()
	data := bytes.Repeat([]byte("ABCD"), 1000) // 4000 bytes
	var sum [32]byte
	tmp := sha256.Sum256(data)
	copy(sum[:], tmp[:])

	offer := frame.FileOfferPayload{
		ID:     1,
		Name:   "x.bin",
		Size:   uint64(len(data)),
		SHA256: sum,
	}
	rcv, err := NewReceiver(offer, dir)
	if err != nil {
		t.Fatal(err)
	}
	// Send in 500-byte chunks to exercise multi-chunk
	chunkSize := 500
	for off := 0; off < len(data); off += chunkSize {
		end := off + chunkSize
		if end > len(data) {
			end = len(data)
		}
		_, err := rcv.WriteChunk(frame.ChunkPayload{
			ID:     1,
			Offset: uint64(off),
			Data:   data[off:end],
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := rcv.Verify(); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "x.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) {
		t.Errorf("file contents differ")
	}
}

func TestReceiverBadHash(t *testing.T) {
	dir := t.TempDir()
	var sum [32]byte // all zeros
	offer := frame.FileOfferPayload{ID: 1, Name: "y", Size: 5, SHA256: sum}
	rcv, _ := NewReceiver(offer, dir)
	rcv.WriteChunk(frame.ChunkPayload{ID: 1, Offset: 0, Data: []byte("hello")})
	if err := rcv.Verify(); err == nil {
		t.Fatal("expected hash mismatch error")
	}
}
