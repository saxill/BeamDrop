package transfer

import (
	"io"
)

// chunkReader wraps a file handle and yields ≤maxBytes slices.
type chunkReader struct {
	f    io.Reader
	size int64
	read int64
}

func newChunkReader(r io.Reader, size int64) *chunkReader {
	return &chunkReader{f: r, size: size}
}

func (c *chunkReader) next(maxBytes int) ([]byte, bool, error) {
	if c.read >= c.size {
		return nil, false, nil
	}
	remaining := c.size - c.read
	n := int64(maxBytes)
	if n > remaining {
		n = remaining
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(c.f, buf); err != nil {
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			// partial final read
			c.read += int64(len(buf))
			return buf, c.read >= c.size, nil
		}
		return nil, false, err
	}
	c.read += n
	more := c.read < c.size
	return buf, more, nil
}
