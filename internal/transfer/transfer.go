// Package transfer implements the sender and receiver halves of the
// file transfer state machine.
package transfer

import (
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"io"
	"mime"
	"os"
	"path/filepath"

	"github.com/saxill/beamdrop/internal/frame"
)

type Sender struct {
	ID   uint64
	Path string
	// Name is what the receiver is told the file is called. It defaults to
	// the base of Path, but a relay stores payloads under an opaque spool
	// id and has to offer the name the file actually arrived with.
	Name   string
	Size   uint64
	SHA256 [32]byte
	MIME   string

	f      *os.File
	cr     *chunkReader
	offset uint64
}

func NewSender(path string) (*Sender, error) {
	return NewSenderNamed(path, filepath.Base(path))
}

// NewSenderNamed reads from path but offers the file under name.
func NewSenderNamed(path, name string) (*Sender, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("sender: stat: %w", err)
	}
	if info.IsDir() {
		return nil, fmt.Errorf("sender: %s is a directory", path)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("sender: open: %w", err)
	}
	// Compute SHA-256 in one pass.
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		f.Close()
		return nil, fmt.Errorf("sender: hash: %w", err)
	}
	var sum [32]byte
	copy(sum[:], h.Sum(nil))
	// Rewind for chunked read.
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		f.Close()
		return nil, fmt.Errorf("sender: seek: %w", err)
	}
	var idBytes [8]byte
	if _, err := rand.Read(idBytes[:]); err != nil {
		f.Close()
		return nil, err
	}
	id := uint64(0)
	for _, b := range idBytes {
		id = id<<8 | uint64(b)
	}
	return &Sender{
		ID:     id,
		Path:   path,
		Name:   filepath.Base(name),
		Size:   uint64(info.Size()),
		SHA256: sum,
		MIME:   mime.TypeByExtension(filepath.Ext(path)),
		f:      f,
		cr:     newChunkReader(f, info.Size()),
	}, nil
}

func (s *Sender) Close() error {
	if s.f != nil {
		return s.f.Close()
	}
	return nil
}

func (s *Sender) Offer() (frame.FileOfferPayload, error) {
	return frame.FileOfferPayload{
		ID:     s.ID,
		Name:   s.Name,
		Size:   s.Size,
		SHA256: s.SHA256,
		MIME:   s.MIME,
	}, nil
}

func (s *Sender) NextChunk(maxBytes int) (frame.ChunkPayload, bool, error) {
	if maxBytes > 65536 {
		maxBytes = 65536
	}
	data, more, err := s.cr.next(maxBytes)
	if err != nil {
		return frame.ChunkPayload{}, false, err
	}
	cp := frame.ChunkPayload{
		ID:     s.ID,
		Offset: s.offset,
		Data:   data,
	}
	s.offset += uint64(len(data))
	return cp, more, nil
}

func (s *Sender) HandleAck(ack frame.AckPayload) {
	// For now, acks are informational. Resume support lands when the
	// receiver signals FILE_ACCEPT with a non-zero ResumeFrom.
	_ = ack
}
