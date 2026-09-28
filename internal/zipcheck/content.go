package zipcheck

import (
	"archive/tar"
	"bufio"
	"compress/bzip2"
	"compress/flate"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
)

// file is a file to read as clamd would unpack it: a member, what a gzip
// unpacks to, a tar entry.
type file struct {
	at string
	// inside is how many archives hold it: 1 for a member of the ZIP.
	inside int
	// size is its size, or -1 when it is known only once read.
	size int64
	// src, when set, reads the file again (a stored member, from its
	// archive), and later postpones a check until the walk that found the
	// file is done.
	src   Source
	later func(func() error)
}

// inspect reads the file r yields to its end, as clamd would unpack it,
// telling what it is by its first bytes, never by its name:
//
//   - a ZIP is checked as the ZIP is (checker.zip), from its own range when
//     it is stored, from memory otherwise (at most MaxBuffer);
//   - a gzip or bzip2 is unpacked, and what it unpacks to is measured
//     against MaxFileSize and MaxScanSize, and read in turn;
//   - a tar's entries are members of their own, each counted and read in
//     turn;
//   - an archive of a format core cannot open is refused as nested;
//   - any other file is searched for archives clamd would unpack from past
//     its first byte (checker.plain).
//
// An archive deeper than Limits.depth is refused as nested.
func (c *checker) inspect(r io.Reader, f file) error {
	br := bufio.NewReaderSize(r, headBytes)
	head, err := br.Peek(headBytes)
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	switch k := kindOf(head); k.open {
	case openZIP:
		return c.innerZIP(br, f)
	case openGzip, openBzip2:
		return c.unpack(br, f, k)
	case openTar:
		return c.tar(br, f)
	case openNone:
		return nested("%s is a %s archive, which core cannot open", f.at, k.name)
	}
	return c.plain(br, f)
}

// deeper refuses an archive nested deeper than core reads: a file inside n
// archives is, as an archive, n deep.
func (c *checker) deeper(f file) error {
	if depth := c.limits.depth(); f.inside > depth {
		return nested("%s is an archive %d deep, deeper than core reads (%d)", f.at, f.inside, max(depth, 0))
	}
	return nil
}

// innerZIP checks a ZIP inside the ZIP: after the walk that found it, from
// its own range, when it is stored; now, from memory, otherwise.
func (c *checker) innerZIP(r *bufio.Reader, f file) error {
	if err := c.deeper(f); err != nil {
		return err
	}
	at := place(f.at)
	if f.src != nil {
		src, size, depth := f.src, f.size, f.inside
		f.later(func() error { return c.zip(src, size, at, depth) })
		return drain(r)
	}
	data, err := c.keep(r, f, "a ZIP")
	if err != nil {
		return err
	}
	return c.zip(Bytes(data), int64(len(data)), at, f.inside)
}

// zip checks a ZIP inside the ZIP, depth archives deep.
func (c *checker) zip(src Source, size int64, at place, depth int) error {
	dir, err := readDirectory(c.ctx, src, size, at, depth, c.b, 0)
	if err != nil {
		return err
	}
	return c.walkZIP(src, dir, at, depth)
}

// embedded checks a ZIP found past the first byte of a file, which must
// end the file: the ZIP src holds from there. Its offsets may count from
// the ZIP or from the file's first byte, stub bytes before it. When no ZIP
// ends the file there (a lone local header, or a ZIP followed by more), it
// is refused as nested; a ZIP there that breaks a rule, as any.
func (c *checker) embedded(src Source, size int64, at place, depth int, stub int64) error {
	dir, err := readDirectory(c.ctx, src, size, at, depth, c.b, stub)
	var r *Refusal
	if errors.As(err, &r) && r.misplaced {
		return nested("%s is no ZIP core can read to the end of the file that holds it", at)
	}
	if err != nil {
		return err
	}
	return c.walkZIP(src, dir, at, depth)
}

// keep reads what r yields into memory, at most MaxBuffer bytes.
func (c *checker) keep(r io.Reader, f file, what string) ([]byte, error) {
	tooBig := nested("%s is %s larger than core keeps in memory to check (%d bytes)", f.at, what, c.limits.MaxBuffer)
	if f.size > c.limits.MaxBuffer {
		return nil, tooBig
	}
	data, err := io.ReadAll(io.LimitReader(r, c.limits.MaxBuffer+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > c.limits.MaxBuffer {
		return nil, tooBig
	}
	return data, drain(r)
}

// unpack reads a gzip or bzip2 file: what it unpacks to is one more file,
// measured against MaxFileSize and MaxScanSize and read in turn. A gzip of
// more than one member is refused as nested: some readers unpack only the
// first.
func (c *checker) unpack(r *bufio.Reader, f file, k kind) error {
	if err := c.deeper(f); err != nil {
		return err
	}
	var inner io.Reader
	if k.open == openGzip {
		zr, err := gzip.NewReader(r)
		if err != nil {
			return corrupt(err, "%s's gzip header is corrupt", f.at)
		}
		defer zr.Close()
		zr.Multistream(false)
		inner = zr
	} else {
		inner = bzip2.NewReader(r)
	}
	if err := c.b.count(1); err != nil {
		return err
	}
	at := "what " + f.at + " unpacks to"
	out := &measure{c: c, at: at}
	if err := c.inspect(io.TeeReader(inner, out), file{at: at, inside: f.inside + 1, size: -1}); err != nil {
		return corrupt(err, "%s's %s data is corrupt or cut short", f.at, k.name)
	}
	if n, err := io.Copy(io.Discard, r); err != nil {
		return err
	} else if n > 0 {
		return nested("%s holds more than one %s stream", f.at, k.name)
	}
	return nil
}

// measure counts what an unpacked file yields against MaxFileSize and the
// budget.
type measure struct {
	c  *checker
	at string
	n  int64
}

func (m *measure) Write(p []byte) (int, error) {
	if err := m.c.alive(); err != nil {
		return 0, err
	}
	m.n += int64(len(p))
	if m.n > m.c.limits.MaxFileSize {
		return 0, tooLarge("%s is more than MaxFileSize (%d bytes)", m.at, m.c.limits.MaxFileSize)
	}
	if err := m.c.b.grow(int64(len(p))); err != nil {
		return 0, err
	}
	return len(p), nil
}

// tar reads a tar's entries: each with content is a member of its own,
// counted toward MaxFiles, held to MaxFileSize and MaxScanSize by the size
// its header gives (the reader yields exactly that), and read in turn.
func (c *checker) tar(r *bufio.Reader, f file) error {
	if err := c.deeper(f); err != nil {
		return err
	}
	tr := tar.NewReader(r)
	for i := 1; ; i++ {
		if err := c.alive(); err != nil {
			return err
		}
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return drain(r)
		}
		if err != nil {
			return corrupt(err, "%s's tar headers are corrupt", f.at)
		}
		switch hdr.Typeflag {
		case tar.TypeDir, tar.TypeSymlink, tar.TypeLink, tar.TypeChar, tar.TypeBlock, tar.TypeFifo:
			continue
		}
		at := fmt.Sprintf("entry %d of %s", i, f.at)
		if err := c.b.count(1); err != nil {
			return err
		}
		if err := c.b.add(at, hdr.Size); err != nil {
			return err
		}
		if err := c.inspect(tr, file{at: at, inside: f.inside + 1, size: hdr.Size}); err != nil {
			return corrupt(err, "%s is corrupt or cut short", at)
		}
	}
}

// plain reads a file that is no archive at its first byte, looking for the
// archives clamd unpacks from past it (a self-extracting program, a ZIP
// appended to an image or a PDF). The first local header there starts a
// ZIP that must end the file, checked as a nested one: after the walk, from
// the file's own range, when the file is stored; from memory (at most
// MaxBuffer) otherwise. Every later local header must be that ZIP's own or
// inside its members' data, which its check holds it to: its members must
// run from that first header to its directory with nothing between them,
// and no local header signature may stand outside their data (so a lone
// one hidden in an extra field or the end record's comment is refused).
// Any other archive past the first byte is refused as nested.
func (c *checker) plain(r io.Reader, f file) error {
	s := scanner{size: f.size}
	zipAt := int64(-1)
	var kept []byte
	hit := func(sig signature, off int64) error {
		if sig.name != "ZIP" {
			return nested("%s holds a %s archive at byte %d, which core cannot open", f.at, sig.name, off)
		}
		if zipAt >= 0 {
			return nil // the ZIP's own local headers
		}
		if err := c.deeper(f); err != nil {
			return err
		}
		zipAt = off
		if f.src != nil {
			src, size, at, depth := window{src: f.src, off: off, size: f.size - off}, f.size-off, embeddedAt(f, off), f.inside
			f.later(func() error { return c.embedded(src, size, at, depth, off) })
			return nil
		}
		kept = append([]byte{}, s.from(off)...)
		return c.fits(kept, f, off)
	}
	chunk := make([]byte, 32<<10)
	for {
		n, err := r.Read(chunk)
		if n > 0 {
			if kept != nil {
				kept = append(kept, chunk[:n]...)
				if err := c.fits(kept, f, zipAt); err != nil {
					return err
				}
			}
			if err := s.feed(chunk[:n], false, hit); err != nil {
				return err
			}
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
	}
	if err := s.feed(nil, true, hit); err != nil {
		return err
	}
	if kept == nil {
		return nil
	}
	return c.embedded(Bytes(kept), int64(len(kept)), embeddedAt(f, zipAt), f.inside, zipAt)
}

func embeddedAt(f file, off int64) place {
	return place(fmt.Sprintf("the ZIP at byte %d of %s", off, f.at))
}

// fits refuses an embedded ZIP kept in memory once it is larger than
// MaxBuffer.
func (c *checker) fits(kept []byte, f file, off int64) error {
	if int64(len(kept)) > c.limits.MaxBuffer {
		return nested("%s holds a ZIP at byte %d larger than core keeps in memory to check (%d bytes)", f.at, off, c.limits.MaxBuffer)
	}
	return nil
}

// corrupt is err as a refusal (invalid) when it says the data is corrupt
// or cut short; any other error (a refusal already, storage, ctx) is
// returned as it is.
func corrupt(err error, format string, args ...any) error {
	var flateErr flate.CorruptInputError
	var bzipErr bzip2.StructuralError
	switch {
	case isRefusal(err):
		return err
	case errors.Is(err, io.ErrUnexpectedEOF), errors.As(err, &flateErr), errors.As(err, &bzipErr),
		errors.Is(err, gzip.ErrChecksum), errors.Is(err, gzip.ErrHeader), errors.Is(err, tar.ErrHeader),
		errors.Is(err, tar.ErrFieldTooLong):
		return invalid(format, args...)
	}
	return err
}
