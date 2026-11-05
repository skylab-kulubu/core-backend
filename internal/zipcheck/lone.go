package zipcheck

import (
	"compress/flate"
	"errors"
	"fmt"
	"io"
)

// loneMembers checks the size bytes src holds, part of a file past a local
// header found past its first byte, when no end record follows them: no
// ZIP there for a reader to open by its directory. clamd opens such a ZIP
// from a local header, by what the header alone says (ZIPSFX). So each
// local header there, from the first byte on, is checked on its own
// (loneMember), counted toward MaxFiles as it is found.
func (c *checker) loneMembers(src Source, size int64, at place, depth int) error {
	var offsets []int64
	add := func(off int64) error {
		if err := c.b.count(1); err != nil {
			return err
		}
		offsets = append(offsets, off)
		return nil
	}
	body, err := src.OpenRange(c.ctx, 0, size)
	if err != nil {
		return err
	}
	r := newStream(body, size)
	s := scanner{size: size, sigs: []signature{localHeaderSignature}}
	hit := func(_ signature, off int64) error { return add(off) }
	chunk := make([]byte, 32<<10)
	first := true
	for err == nil {
		if err = c.alive(); err != nil {
			break
		}
		n, rerr := r.Read(chunk)
		if first && n > 0 {
			first = false
			// The scanner reports signatures past its first byte only.
			if head := chunk[:n]; localHeaderSignature.matches(head, size) {
				err = add(0)
			}
		}
		if err == nil && n > 0 {
			err = s.feed(chunk[:n], false, hit)
		}
		if err == nil && errors.Is(rerr, io.EOF) {
			err = s.feed(nil, true, hit)
			break
		}
		if err == nil {
			err = rerr
		}
	}
	body.Close()
	if err != nil {
		return err
	}
	for _, off := range offsets {
		if err := c.loneMember(src, size, off, at, depth); err != nil {
			return err
		}
	}
	return nil
}

// errLonger is a deflate stream that inflates to more than its header says.
var errLonger = errors.New("zipcheck: inflates past its declared size")

// errBroken is a deflate stream that is corrupt, or cut short by its
// compressed size.
var errBroken = errors.New("zipcheck: the deflate stream breaks off")

// loneMember checks the local header at off of the size bytes src holds,
// with no directory to go by. One that is no local header clamd unpacks (an
// undefined method, a name or data that runs past the file) is chance
// bytes, and passes. Any other passes only when clamd unpacks all of it by
// what the header says, and what it unpacks to passes as a member's content
// (checker.inspect: the limits, archives inside): no data descriptor, not
// encrypted, stored or deflated, both sizes above zero; stored, the sizes
// equal; deflated, the stream ending within its compressed size and
// inflating to exactly its size. Data that is no deflate stream from its
// first bits (corrupt before it yields a byte) is chance bytes too: no
// reader unpacks anything from it. Any other is refused as nested, its
// header's fields in the reason.
func (c *checker) loneMember(src Source, size, off int64, at place, depth int) error {
	if err := c.alive(); err != nil {
		return err
	}
	if size-off < localLen {
		return nil
	}
	h, err := readAt(c.ctx, src, off, localLen)
	if err != nil {
		return err
	}
	if !unpackableLocalHeader(h, size-off) {
		return nil
	}
	flags, method := le16(h[6:]), le16(h[8:])
	csize, usize := int64(le32(h[18:])), int64(le32(h[22:]))
	m := fmt.Sprintf("the local header %d bytes past the start of %s", off, at)
	refuse := func(why string) error {
		return nested("%s %s, which clamd would not unpack whole (method %d, flags %#04x, csize %d, usize %d)", m, why, method, flags, csize, usize)
	}
	switch {
	case flags&flagsEncrypted != 0:
		return refuse("is encrypted")
	case flags&flagDescriptor != 0:
		return refuse("leaves its sizes to a data descriptor")
	case method != methodStore && method != methodDeflate:
		return refuse("is compressed with another method than stored or deflate")
	case csize == 0 || usize == 0:
		return refuse("has a size of zero")
	case method == methodStore && csize != usize:
		return refuse("is stored with sizes that differ")
	}
	data := off + localLen + int64(le16(h[26:])) + int64(le16(h[28:]))
	body, err := src.OpenRange(c.ctx, data, csize)
	if err != nil {
		return err
	}
	defer body.Close()
	f := file{at: m, inside: depth + 1, size: usize}
	if method == methodStore {
		return c.inspect(io.TeeReader(newStream(body, csize), &measure{c: c, at: m}), f)
	}
	inflated := flate.NewReader(newStream(body, csize))
	defer inflated.Close()
	d := &declared{r: inflated, usize: usize}
	err = c.inspect(io.TeeReader(d, &measure{c: c, at: m}), f)
	switch {
	case d.corrupt && d.n == 0:
		return nil
	case d.corrupt:
		return refuse(fmt.Sprintf("has a deflate stream that breaks off after %d bytes", d.n))
	case d.short:
		return refuse("has a deflate stream that runs past its compressed size")
	case d.longer:
		return refuse("inflates to more than its size")
	case err != nil:
		return err
	case d.n != usize:
		return refuse(fmt.Sprintf("inflates to %d bytes, not its size", d.n))
	}
	return nil
}

// declared reads a lone member's deflate stream, noting whether it is
// corrupt, cut short by its compressed size, or inflates past the size its
// header declares.
type declared struct {
	r       io.Reader
	usize   int64
	n       int64
	corrupt bool
	short   bool
	longer  bool
}

func (d *declared) Read(p []byte) (int, error) {
	n, err := d.r.Read(p)
	d.n += int64(n)
	if d.n > d.usize {
		d.longer = true
		return n, errLonger
	}
	var corrupt flate.CorruptInputError
	switch {
	case errors.As(err, &corrupt):
		d.corrupt = true
		return n, errBroken
	case errors.Is(err, io.ErrUnexpectedEOF):
		d.short = true
		return n, errBroken
	}
	return n, err
}
