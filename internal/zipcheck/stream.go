package zipcheck

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// maxReadBytes bounds one ranged read of a directory.
const maxReadBytes = 1 << 20

// readAt reads the n bytes of src from off, at most maxReadBytes a ranged
// read.
func readAt(ctx context.Context, src Source, off, n int64) ([]byte, error) {
	out := make([]byte, n)
	for done := int64(0); done < n; {
		step := min(n-done, maxReadBytes)
		body, err := src.OpenRange(ctx, off+done, step)
		if err != nil {
			return nil, err
		}
		_, err = io.ReadFull(body, out[done:done+step])
		body.Close()
		if err != nil {
			return nil, fmt.Errorf("zipcheck: read %d bytes at %d: %w", step, off+done, err)
		}
		done += step
	}
	return out, nil
}

// Bytes is a file held in memory, as a Source: a private file decrypted to
// be checked, or a ZIP inside the ZIP read again.
func Bytes(data []byte) Source { return memory(data) }

type memory []byte

func (m memory) OpenRange(_ context.Context, off, n int64) (io.ReadCloser, error) {
	if off < 0 || n <= 0 || off+n > int64(len(m)) {
		return nil, fmt.Errorf("zipcheck: bytes %d+%d are outside the %d held", off, n, len(m))
	}
	return io.NopCloser(bytes.NewReader(m[off : off+n])), nil
}

// window is the size bytes of a Source from off: a stored member, read
// again from the file that holds it.
type window struct {
	src       Source
	off, size int64
}

func (w window) OpenRange(ctx context.Context, off, n int64) (io.ReadCloser, error) {
	if off < 0 || n <= 0 || off+n > w.size {
		return nil, fmt.Errorf("zipcheck: bytes %d+%d are outside the %d of a member", off, n, w.size)
	}
	return w.src.OpenRange(ctx, w.off+off, n)
}

// errShort is a file that ends before its size: storage served less than it
// holds, which says nothing about the ZIP.
var errShort = errors.New("zipcheck: the ZIP ended before its size")

// stream is an archive read in order, knowing where it is.
type stream struct {
	r    *bufio.Reader
	pos  int64
	size int64
}

func newStream(r io.Reader, size int64) *stream {
	return &stream{r: bufio.NewReaderSize(r, 64<<10), size: size}
}

func (s *stream) Read(p []byte) (int, error) {
	n, err := s.r.Read(p)
	s.pos += int64(n)
	if errors.Is(err, io.EOF) && s.pos < s.size {
		err = errShort
	}
	return n, err
}

func (s *stream) ReadByte() (byte, error) {
	c, err := s.r.ReadByte()
	if err == nil {
		s.pos++
	} else if errors.Is(err, io.EOF) && s.pos < s.size {
		err = errShort
	}
	return c, err
}

// next reads exactly n bytes.
func (s *stream) next(n int) ([]byte, error) {
	out := make([]byte, n)
	if _, err := io.ReadFull(s, out); err != nil {
		if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
			return nil, errShort
		}
		return nil, err
	}
	return out, nil
}

func (s *stream) skip(n int64) error {
	if _, err := io.CopyN(io.Discard, s, n); err != nil {
		if errors.Is(err, io.EOF) {
			return errShort
		}
		return err
	}
	return nil
}

// section is the next left bytes of the stream: a member's data. It is a
// flate.Reader, so the inflater reads no byte past the deflate stream.
type section struct {
	s    *stream
	left int64
}

func (d *section) Read(p []byte) (int, error) {
	if d.left <= 0 {
		return 0, io.EOF
	}
	if int64(len(p)) > d.left {
		p = p[:d.left]
	}
	n, err := d.s.Read(p)
	d.left -= int64(n)
	return n, err
}

func (d *section) ReadByte() (byte, error) {
	if d.left <= 0 {
		return 0, io.EOF
	}
	c, err := d.s.ReadByte()
	if err == nil {
		d.left--
	}
	return c, err
}

// drain reads r to its end.
func drain(r io.Reader) error {
	_, err := io.Copy(io.Discard, r)
	return err
}

func le16(b []byte) uint16 { return binary.LittleEndian.Uint16(b) }
func le32(b []byte) uint32 { return binary.LittleEndian.Uint32(b) }
func le64(b []byte) uint64 { return binary.LittleEndian.Uint64(b) }
