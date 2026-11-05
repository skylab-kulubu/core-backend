package zipcheck

import (
	"compress/flate"
	"errors"
	"fmt"
	"io"
)

// loneMembers checks a file past whose first byte a local header was found,
// with no end record at the file's end: src holds the file from that first
// header on (size bytes), at, depth archives deep. No reader opens a ZIP
// from such a file's end; clamd unpacks each local header it finds in it on
// its own, with no directory, and reads only up to MaxFileSize of what one
// inflates to (checked: a lone one appended to a PDF is unpacked, and one
// past MaxFileSize read only in part). So each local header there, the
// first and every later one, is a member of its own: counted toward
// MaxFiles, what it unpacks to held to MaxFileSize and MaxScanSize, and
// read in turn (loneMember). A local header signature that is no local
// header clamd unpacks (an undefined method, data past the file) is none,
// as anywhere.
//
// Such a header is what a PDF holds when it carries a whole ZIP in its
// middle (an embedded file stored as it is), or, by chance, in its other
// data: what lies there is checked, not refused for where it lies.
func (c *checker) loneMembers(src Source, size int64, at place, depth int) error {
	offsets := []int64{0}
	s := scanner{size: size, sigs: []signature{localHeaderSignature}}
	hit := func(_ signature, off int64) error {
		offsets = append(offsets, off)
		return nil
	}
	body, err := src.OpenRange(c.ctx, 0, size)
	if err != nil {
		return err
	}
	r := newStream(body, size)
	chunk := make([]byte, 32<<10)
	for {
		if err = c.alive(); err != nil {
			break
		}
		n, rerr := r.Read(chunk)
		if n > 0 {
			if err = s.feed(chunk[:n], false, hit); err != nil {
				break
			}
		}
		if errors.Is(rerr, io.EOF) {
			err = s.feed(nil, true, hit)
			break
		}
		if rerr != nil {
			err = rerr
			break
		}
	}
	body.Close()
	if err != nil {
		return err
	}
	if err := c.b.count(len(offsets)); err != nil {
		return err
	}
	for _, off := range offsets {
		if err := c.loneMember(src, size, off, at, depth); err != nil {
			return err
		}
	}
	return nil
}

// loneMember checks the local header at off of src (size bytes) as clamd
// unpacks one no directory lists, by what the header alone says:
//
//   - a deflated member is inflated to the end of its deflate stream,
//     whatever its sizes say (at least what clamd inflates from its csize
//     bytes, or a reader from the sizes a directory elsewhere gives), and
//     what it inflates to is read as a member is (checker.inspect);
//   - a stored member is its csize bytes, read as a member is; and since
//     clamd inflates a stored one whose sizes say it grows, and a
//     directory elsewhere may call it deflated, its data is inflated as a
//     deflated member's too;
//   - an encrypted member, one compressed with another method, or a stored
//     one whose size is left to a data descriptor, holds what core cannot
//     measure: refused as nested.
//
// A deflate stream that breaks off ends the member there: clamd unpacks
// nothing from it (its extraction fails), and no reader inflates more. What
// came before the break is still held to the limits. So data that only
// starts like a member (chance bytes in a PDF) passes.
func (c *checker) loneMember(src Source, size, off int64, at place, depth int) error {
	if err := c.alive(); err != nil {
		return err
	}
	m := fmt.Sprintf("the lone member at byte %d of %s", off, at)
	h, err := readAt(c.ctx, src, off, localLen)
	if err != nil {
		return err
	}
	flags, method := le16(h[6:]), le16(h[8:])
	csize := int64(le32(h[18:]))
	data := off + localLen + int64(le16(h[26:])) + int64(le16(h[28:]))
	switch {
	case flags&flagsEncrypted != 0:
		return nested("%s is encrypted, which core cannot read", m)
	case method != methodStore && method != methodDeflate:
		return nested("%s is compressed with method %d, which core cannot read", m, method)
	case method == methodStore && flags&flagDescriptor != 0:
		return nested("%s is stored with its size left to a data descriptor, which core cannot tell", m)
	}
	if method == methodStore && csize > 0 {
		body, err := src.OpenRange(c.ctx, data, csize)
		if err != nil {
			return err
		}
		err = c.inspect(io.TeeReader(newStream(body, csize), &measure{c: c, at: m}), file{at: m, inside: depth + 1, size: csize})
		body.Close()
		if err != nil {
			return err
		}
	}
	return c.loneInflated(src, data, size-data, m, depth)
}

// loneInflated inflates the n bytes of src from data, a lone member's,
// up to the end of their deflate stream or where it breaks off, and reads
// what they inflate to as a member, held to the limits.
func (c *checker) loneInflated(src Source, data, n int64, m string, depth int) error {
	if n <= 0 {
		return nil
	}
	body, err := src.OpenRange(c.ctx, data, n)
	if err != nil {
		return err
	}
	defer body.Close()
	inflated := flate.NewReader(newStream(body, n))
	defer inflated.Close()
	r := &untilBroken{r: inflated}
	return c.inspect(io.TeeReader(r, &measure{c: c, at: m}), file{at: m, inside: depth + 1, size: -1})
}

// untilBroken ends a deflate stream where it breaks off (corrupt, or cut
// short by the end of the file): what came before is all it holds. Any
// other error (storage serving less than it holds, ctx) is returned as it
// is.
type untilBroken struct{ r io.Reader }

func (u *untilBroken) Read(p []byte) (int, error) {
	n, err := u.r.Read(p)
	var corrupt flate.CorruptInputError
	if errors.As(err, &corrupt) || errors.Is(err, io.ErrUnexpectedEOF) {
		err = io.EOF
	}
	return n, err
}
