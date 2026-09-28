package zipcheck

import (
	"bytes"
	"compress/flate"
	"crypto/sha256"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
)

// walkZIP streams the ZIP src holds, whose directory is dir, and checks
// every member, then the archives found in stored members, which it reads
// again from their own ranges.
func (c *checker) walkZIP(src Source, dir *directory, at place, depth int) error {
	if len(dir.entries) == 0 {
		return nil
	}
	body, err := src.OpenRange(c.ctx, 0, dir.size)
	if err != nil {
		return err
	}
	w := &walker{c: c, s: newStream(body, dir.size), src: src, at: at, depth: depth, copy: make([]byte, 32<<10)}
	err = w.walk(dir)
	body.Close()
	if err != nil {
		return err
	}
	for _, check := range w.later {
		if err := check(); err != nil {
			return err
		}
	}
	return nil
}

// walker reads an archive in order, from its first byte to its last.
type walker struct {
	c *checker
	s *stream
	// src is the archive, to read a stored member again from.
	src   Source
	at    place
	depth int
	copy  []byte
	// later are the checks of archives in stored members, made once the
	// walk is done.
	later []func() error
}

func (w *walker) postpone(check func() error) { w.later = append(w.later, check) }

// walk checks every member against the directory, then that the archive
// ends with the directory as it was read.
func (w *walker) walk(dir *directory) error {
	for i, e := range dir.entries {
		next := dir.start
		if i+1 < len(dir.entries) {
			next = dir.entries[i+1].offset
		}
		if err := w.member(e, next); err != nil {
			return err
		}
	}
	sum := sha256.New()
	if _, err := io.CopyN(sum, w.s, dir.size-dir.start); err != nil {
		return err
	}
	if !bytes.Equal(sum.Sum(nil), dir.endSum[:]) {
		return fmt.Errorf("zipcheck: %s's directory changed while it was read", w.at.archive())
	}
	if _, err := w.s.r.ReadByte(); !errors.Is(err, io.EOF) {
		if err == nil {
			err = fmt.Errorf("zipcheck: %s is longer than its size", w.at.archive())
		}
		return err
	}
	return nil
}

// member checks one member: its local header, its data and what it holds,
// and what follows it up to next, where the next member (or the directory)
// starts.
func (w *walker) member(e entry, next int64) error {
	if err := w.c.alive(); err != nil {
		return err
	}
	m := w.at.member(e.index)
	if w.s.pos != e.offset {
		return invalid("bytes before %s belong to no member", m)
	}
	if err := w.localHeader(e, m); err != nil {
		return err
	}
	offset := w.s.pos
	if offset+e.csize > next {
		return invalid("%s overlaps what follows it", m)
	}
	if err := w.content(e, m, offset); err != nil {
		return err
	}
	if e.flags&flagDescriptor != 0 {
		if err := w.descriptor(e, m, next); err != nil {
			return err
		}
	}
	if w.s.pos != next {
		return invalid("bytes after %s belong to no member", m)
	}
	return nil
}

// localHeader reads the member's local header, which must agree with the
// directory: its flags, method and name, and, without a data descriptor,
// its checksum and sizes (with one, it gives them or leaves them zero).
func (w *walker) localHeader(e entry, m string) error {
	h, err := w.s.next(localLen)
	if err != nil {
		return err
	}
	if le32(h) != localSig {
		return invalid("%s has no local header where the directory says", m)
	}
	disagrees := invalid("%s's local header disagrees with the directory", m)
	nameLen, extraLen := int(le16(h[26:])), int64(le16(h[28:]))
	if (le16(h[6:])^e.flags)&(flagsEncrypted|flagDescriptor) != 0 || le16(h[8:]) != e.method || nameLen != len(e.name) {
		return disagrees
	}
	name, err := w.s.next(nameLen)
	if err != nil {
		return err
	}
	if string(name) != e.name {
		return disagrees
	}
	crc, csize, usize := le32(h[14:]), le32(h[18:]), le32(h[22:])
	if csize == 0xffffffff || usize == 0xffffffff {
		return invalid("%s has ZIP64 sizes, which clamd does not read", m)
	}
	given := func(v uint32, want int64) bool { return int64(v) == want || (e.flags&flagDescriptor != 0 && v == 0) }
	if !given(crc, int64(e.crc)) || !given(csize, e.csize) || !given(usize, e.usize) {
		return disagrees
	}
	return w.s.skip(extraLen)
}

// content streams the member's data, inflated, through its checks: what it
// holds (checker.inspect) and what it declares (sink). A stored member
// can be read again from the archive.
func (w *walker) content(e entry, m string, offset int64) error {
	data := &section{s: w.s, left: e.csize}
	out := &sink{c: w.c, member: m, declared: e.usize}
	f := file{at: m, inside: w.depth + 1, size: e.usize}
	var plain io.Reader = data
	if e.method == methodDeflate {
		inflated := flate.NewReader(data)
		defer inflated.Close()
		plain = inflated
	} else {
		f.src, f.later = window{src: w.src, off: offset, size: e.usize}, w.postpone
	}
	if err := w.c.inspect(io.TeeReader(plain, out), f); err != nil {
		return corrupt(err, "%s's data is corrupt or cut short", m)
	}
	if data.left != 0 {
		return invalid("%s has bytes after its deflate stream", m)
	}
	if out.n != e.usize || out.crc != e.crc {
		return invalid("%s inflates to other bytes than it declares", m)
	}
	return nil
}

// descriptor checks the data descriptor that ends a member written with
// one: 12 bytes, or 16 with its signature, that repeat the directory.
func (w *walker) descriptor(e entry, m string, next int64) error {
	var d []byte
	var err error
	switch next - w.s.pos {
	case 16:
		if d, err = w.s.next(16); err != nil {
			return err
		}
		if le32(d) != descriptorSig {
			return invalid("%s has no data descriptor where one should be", m)
		}
		d = d[4:]
	case 12:
		if d, err = w.s.next(12); err != nil {
			return err
		}
	default:
		return invalid("%s has no data descriptor where one should be", m)
	}
	if le32(d) != e.crc || int64(le32(d[4:])) != e.csize || int64(le32(d[8:])) != e.usize {
		return invalid("%s's data descriptor disagrees with the directory", m)
	}
	return nil
}

// sink takes a member's inflated bytes, never more than it declares, and
// sums their checksum.
type sink struct {
	c        *checker
	member   string
	declared int64
	n        int64
	crc      uint32
}

func (k *sink) Write(p []byte) (int, error) {
	if err := k.c.alive(); err != nil {
		return 0, err
	}
	if int64(len(p)) > k.declared-k.n {
		return 0, invalid("%s inflates to more than the %d bytes it declares", k.member, k.declared)
	}
	k.n += int64(len(p))
	k.crc = crc32.Update(k.crc, crc32.IEEETable, p)
	return len(p), nil
}
