package transfer

import (
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/saxill/beamdrop/internal/frame"
)

type Receiver struct {
	ID     uint64
	Dest   string
	Name   string
	Size   uint64
	SHA256 [32]byte

	f       *os.File
	written uint64
}

func NewReceiver(offer frame.FileOfferPayload, destDir string) (*Receiver, error) {
	// Avoid path traversal: take only the base name.
	safe := filepath.Base(offer.Name)
	full := filepath.Join(destDir, safe)
	f, err := os.OpenFile(full, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return nil, fmt.Errorf("receiver: open: %w", err)
	}
	return &Receiver{
		ID:     offer.ID,
		Dest:   full,
		Name:   safe,
		Size:   offer.Size,
		SHA256: offer.SHA256,
		f:      f,
	}, nil
}

// WriteChunk writes at the given offset, returning the new highest contiguous
// offset (0 if this chunk is not contiguous with what came before).
func (r *Receiver) WriteChunk(c frame.ChunkPayload) (uint64, error) {
	if c.ID != r.ID {
		return r.written, fmt.Errorf("receiver: id mismatch %d != %d", c.ID, r.ID)
	}
	if c.Offset != r.written {
		return r.written, nil // not contiguous; sender should re-offer
	}
	if _, err := r.f.WriteAt(c.Data, int64(c.Offset)); err != nil {
		return r.written, fmt.Errorf("receiver: write: %w", err)
	}
	r.written += uint64(len(c.Data))
	return r.written, nil
}

// Written returns the highest contiguous byte offset written so far.
func (r *Receiver) Written() uint64 { return r.written }

func (r *Receiver) Verify() error {
	if err := r.f.Sync(); err != nil {
		return err
	}
	if err := r.f.Close(); err != nil {
		return err
	}
	r.f = nil
	f, err := os.Open(r.Dest)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := copyAll(h, f); err != nil {
		return err
	}
	var got [32]byte
	copy(got[:], h.Sum(nil))
	if got != r.SHA256 {
		_ = os.Remove(r.Dest)
		return fmt.Errorf("receiver: hash mismatch")
	}
	return nil
}

func copyAll(dst io.Writer, src io.Reader) (int64, error) {
	return io.Copy(dst, src)
}
