// Package envelope encrypts a file with a 256-bit data key so that it can be
// written and read as a stream in constant memory: AES-256-GCM over 64 KiB
// segments, in the STREAM construction (Hoang, Reyhanitabar, Rogaway and
// Vizár, 2015) that age and Tink's streaming AEAD also follow.
//
// The format (Algorithm) is:
//
//	header   magic "SKYM" | format version 0x01 | segment size, uint32 big endian | nonce prefix, 7 random bytes
//	segment  AES-256-GCM of up to SegmentSize plaintext bytes, followed by its 16-byte tag
//	AAD      the header, then the associated data (a stored object's key)
//
// Every segment is SegmentSize plaintext bytes except the last, which holds
// the rest (0 to SegmentSize bytes; an empty file is one empty segment). A
// segment's nonce is the nonce prefix, the segment's index as a uint32 big
// endian, and one byte that is 1 for the last segment and 0 otherwise. Every
// segment's additional data is the header followed by the caller's
// associated data (for a stored object, its key). So a changed byte
// anywhere, a segment moved, dropped or added, a file cut at any point, or a
// file copied to another object fails the check of some segment, and no
// plaintext of a segment that fails is ever released. A reader that fails
// part way has released only the verified segments before it.
package envelope

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
)

// Algorithm names this format on the records that store its ciphertext.
const Algorithm = "aes-256-gcm-chunked-v1"

const (
	// SegmentSize is the plaintext size of every segment but the last.
	SegmentSize = 64 << 10
	// HeaderSize is the size of the header in front of the first segment.
	HeaderSize = 16

	tagSize         = 16
	noncePrefixSize = 7
	formatVersion   = 1
)

var magic = [4]byte{'S', 'K', 'Y', 'M'}

// ErrCorrupt is ciphertext that is not what NewWriter wrote under this key
// and associated data: changed, cut short, extended, reordered, sealed with
// another key, or for another object.
var ErrCorrupt = errors.New("envelope: ciphertext is corrupt or was changed")

// CorruptError is a segment that failed its check. errors.Is matches
// ErrCorrupt.
type CorruptError struct {
	// Segment is the index of the segment, from 0; -1 for the header.
	Segment int
	Reason  string
}

func (e *CorruptError) Error() string {
	if e.Segment < 0 {
		return fmt.Sprintf("%v: header: %s", ErrCorrupt, e.Reason)
	}
	return fmt.Sprintf("%v: segment %d: %s", ErrCorrupt, e.Segment, e.Reason)
}

func (e *CorruptError) Unwrap() error { return ErrCorrupt }

// ErrKeySize is a data key that is not 256 bits.
var ErrKeySize = errors.New("envelope: the data key is not 32 bytes")

func newAEAD(key []byte) (cipher.AEAD, error) {
	if len(key) != 32 {
		return nil, ErrKeySize
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

type segmentNonce struct {
	prefix  [noncePrefixSize]byte
	counter uint32
}

// next is the nonce of the next segment. It fails only past 2^32 segments
// (256 TiB), where the counter would repeat.
func (n *segmentNonce) next(final bool) ([]byte, error) {
	if n.counter == math.MaxUint32 {
		return nil, errors.New("envelope: file too large")
	}
	nonce := make([]byte, 12)
	copy(nonce, n.prefix[:])
	binary.BigEndian.PutUint32(nonce[noncePrefixSize:], n.counter)
	if final {
		nonce[11] = 1
	}
	n.counter++
	return nonce, nil
}

type writer struct {
	dst    io.Writer
	aead   cipher.AEAD
	aad    []byte
	nonce  segmentNonce
	buf    []byte
	out    []byte
	err    error
	closed bool
}

// NewWriter writes the header to dst and returns a writer that encrypts what
// is written to it into dst, one segment at a time, bound to associated (a
// stored object's key). Close writes the last segment; a file whose writer is
// not closed cannot be opened.
func NewWriter(dst io.Writer, key, associated []byte) (io.WriteCloser, error) {
	aead, err := newAEAD(key)
	if err != nil {
		return nil, err
	}
	w := &writer{dst: dst, aead: aead, buf: make([]byte, 0, SegmentSize), out: make([]byte, 0, SegmentSize+tagSize)}
	if _, err := rand.Read(w.nonce.prefix[:]); err != nil {
		return nil, err
	}
	header := make([]byte, 0, HeaderSize)
	header = append(header, magic[:]...)
	header = append(header, formatVersion)
	header = binary.BigEndian.AppendUint32(header, SegmentSize)
	header = append(header, w.nonce.prefix[:]...)
	if _, err := dst.Write(header); err != nil {
		return nil, err
	}
	w.aad = append(header, associated...)
	return w, nil
}

func (w *writer) Write(p []byte) (int, error) {
	if w.err != nil {
		return 0, w.err
	}
	if w.closed {
		return 0, errors.New("envelope: write after close")
	}
	written := 0
	for len(p) > 0 {
		// A full segment is sealed only once more data follows, so that the
		// last segment is always sealed as the last one.
		if len(w.buf) == SegmentSize {
			if err := w.seal(false); err != nil {
				return written, err
			}
		}
		n := copy(w.buf[len(w.buf):SegmentSize], p)
		w.buf = w.buf[:len(w.buf)+n]
		p = p[n:]
		written += n
	}
	return written, nil
}

func (w *writer) Close() error {
	if w.closed {
		return w.err
	}
	w.closed = true
	if w.err != nil {
		return w.err
	}
	return w.seal(true)
}

func (w *writer) seal(final bool) error {
	nonce, err := w.nonce.next(final)
	if err != nil {
		w.err = err
		return err
	}
	w.out = w.aead.Seal(w.out[:0], nonce, w.buf, w.aad)
	if _, err := w.dst.Write(w.out); err != nil {
		w.err = err
		return err
	}
	w.buf = w.buf[:0]
	return nil
}

type reader struct {
	src     io.Reader
	aead    cipher.AEAD
	aad     []byte
	nonce   segmentNonce
	buf     []byte
	carried int
	plain   []byte
	pending []byte
	done    bool
	err     error
}

// NewReader reads and checks the header from src and returns a reader of the
// plaintext, which must have been written with the same associated data.
// Every segment is checked before any of its plaintext is returned; a check
// that fails is a *CorruptError.
func NewReader(src io.Reader, key, associated []byte) (io.Reader, error) {
	aead, err := newAEAD(key)
	if err != nil {
		return nil, err
	}
	header := make([]byte, HeaderSize)
	if _, err := io.ReadFull(src, header); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return nil, &CorruptError{Segment: -1, Reason: "no header"}
		}
		return nil, err
	}
	if !bytes.Equal(header[:4], magic[:]) || header[4] != formatVersion || binary.BigEndian.Uint32(header[5:9]) != SegmentSize {
		return nil, &CorruptError{Segment: -1, Reason: "unknown header"}
	}
	r := &reader{
		src: src, aead: aead, aad: append(header, associated...),
		buf:   make([]byte, SegmentSize+tagSize+1),
		plain: make([]byte, 0, SegmentSize),
	}
	copy(r.nonce.prefix[:], header[9:])
	return r, nil
}

func (r *reader) Read(p []byte) (int, error) {
	for len(r.pending) == 0 {
		if r.err != nil {
			return 0, r.err
		}
		if r.done {
			return 0, io.EOF
		}
		if err := r.nextSegment(); err != nil {
			r.err = err
		}
	}
	n := copy(p, r.pending)
	r.pending = r.pending[n:]
	return n, nil
}

// nextSegment reads one segment and a byte past it: a segment followed by
// more data is not the last one, whatever it claims.
func (r *reader) nextSegment() error {
	full := SegmentSize + tagSize
	n, err := io.ReadFull(r.src, r.buf[r.carried:full+1])
	total := r.carried + n
	final := false
	switch {
	case err == nil:
		// A whole segment and the first byte of the next.
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		final = true
	default:
		return err
	}
	segment := r.buf[:min(total, full)]
	index := int(r.nonce.counter)
	if len(segment) < tagSize {
		return &CorruptError{Segment: index, Reason: "cut short"}
	}
	nonce, err := r.nonce.next(final)
	if err != nil {
		return err
	}
	plain, err := r.aead.Open(r.plain[:0], nonce, segment, r.aad)
	if err != nil {
		return &CorruptError{Segment: index, Reason: "failed its check"}
	}
	r.pending = plain
	if final {
		r.done = true
		return nil
	}
	r.buf[0] = r.buf[full]
	r.carried = 1
	return nil
}
