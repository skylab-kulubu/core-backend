// Package faststart moves an MP4's moov box in front of its media data, so
// a player can start before the whole file has arrived (media redesign
// ticket 13). It is qt-faststart's method, in Go: no ffmpeg, no re-encode.
// The samples stay byte for byte; only the boxes move.
//
//   - The file's top-level boxes are read by bounded ranged reads (32- and
//     64-bit sizes, and a last box whose size 0 runs to the end of the
//     file). A file whose moov already comes before its first mdat needs
//     nothing (ErrAlreadyFaststart).
//   - The moov is read whole, up to Limits.MaxMoovBytes, and walked down
//     moov/trak/mdia/minf/stbl to every track's chunk offset table (stco,
//     or co64 for 64-bit offsets). Every other box is kept as it is.
//   - The rewritten file (Layout) is the source's bytes up to its first
//     mdat, the rewritten moov, the source's bytes from that mdat up to the
//     old moov, and whatever followed the old moov. Every chunk offset moves
//     with the bytes it points into: by the new moov's size for the media
//     data, by the moov's growth for what followed it. A chunk offset that
//     points anywhere else is refused.
//   - An stco whose offsets would pass 4 GiB once moved is upgraded to a
//     co64, every stco of the moov at once as qt-faststart does, and the
//     moov's size (and so the shift) is worked out again.
//
// A file it cannot rewrite safely is refused (ErrInvalid, with the reason):
// a malformed box, too many boxes, a moov larger than the limit, a
// compressed moov (cmov), a fragmented file (moof, mvex, ...), sample
// auxiliary offsets (saio), item locations (an iloc in a meta box anywhere
// the walk enters, in a udta or meco there too, or a top-level meta or
// meco), samples in another file (a data reference that is not
// self-contained). Such a file still plays, once it
// has been downloaded.
//
// See "Video faststart" in docs/media-lifecycle.md.
package faststart

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
)

// Source is a stored file read by byte ranges (ranged GETs).
type Source interface {
	// OpenRange streams the n bytes of the file from off.
	OpenRange(ctx context.Context, off, n int64) (io.ReadCloser, error)
}

var (
	// ErrAlreadyFaststart is a file whose moov already comes before its
	// media data (or that has none): there is nothing to move.
	ErrAlreadyFaststart = errors.New("faststart: the moov box already comes before the media data")
	// ErrInvalid is a file the rewrite refuses (Refusal says why). It
	// still plays once downloaded whole.
	ErrInvalid = errors.New("faststart: the file cannot be rewritten")
	// ErrMismatch is a stored rewrite that is not the one planned (Verify).
	ErrMismatch = errors.New("faststart: the stored rewrite is not the one planned")
)

// Refusal is a file the rewrite refuses, and why. The reason names boxes
// by their type and place in the file, never anything they hold. errors.Is
// matches ErrInvalid.
type Refusal struct {
	Reason string
}

func (r *Refusal) Error() string { return ErrInvalid.Error() + ": " + r.Reason }

func (r *Refusal) Unwrap() error { return ErrInvalid }

func invalid(format string, args ...any) error {
	return &Refusal{Reason: fmt.Sprintf(format, args...)}
}

// Limits bound what a rewrite reads and keeps.
type Limits struct {
	// MaxMoovBytes is the largest moov read into memory, before and after
	// the rewrite (an stco upgraded to a co64 grows it).
	MaxMoovBytes int64
	// MaxTopLevelBoxes bounds the boxes the file is walked through, one
	// ranged read at most each.
	MaxTopLevelBoxes int
	// MaxMoovBoxes bounds the boxes read inside the moov.
	MaxMoovBoxes int
}

// DefaultLimits: a 64 MiB moov (a two-hour video's is a few MiB), 256
// top-level boxes (a camera writes three or four), 100 000 boxes in the
// moov.
var DefaultLimits = Limits{MaxMoovBytes: 64 << 20, MaxTopLevelBoxes: 256, MaxMoovBoxes: 100_000}

func (l Limits) validate() error {
	if l.MaxMoovBytes <= 0 || l.MaxTopLevelBoxes <= 0 || l.MaxMoovBoxes <= 0 {
		return errors.New("faststart: every limit must be positive")
	}
	return nil
}

// fragmentBoxes are the top-level boxes of a fragmented MP4, whose
// fragments may name absolute offsets the rewrite does not move.
var fragmentBoxes = map[string]bool{"moof": true, "mfra": true, "sidx": true, "ssix": true}

// Layout is a planned rewrite: the rewritten file as runs of the source's
// bytes around the rewritten moov.
type Layout struct {
	// Size is the rewritten file's size.
	Size int64
	// MoovAt is where the rewritten moov starts in it.
	MoovAt int64

	segments []segment
	moov     []byte
	probes   []probe
}

// segment is a run of the rewritten file: the rewritten moov (data), or
// the source's bytes from from.
type segment struct {
	at, n int64
	data  []byte
	from  int64
}

// probe is a chunk offset before and after the rewrite, and how many of its
// bytes to compare (Verify).
type probe struct {
	from, to, n int64
}

// Plan plans the rewrite of the file of size bytes at src. It reads only
// the top-level box headers and the moov. ErrAlreadyFaststart when there is
// nothing to move; a *Refusal (ErrInvalid) for a file it cannot rewrite;
// any other error is the source's.
func Plan(ctx context.Context, src Source, size int64, limits Limits) (Layout, error) {
	if err := limits.validate(); err != nil {
		return Layout{}, err
	}
	if size < 0 || size > math.MaxInt64/2 {
		return Layout{}, invalid("a file of %d bytes", size)
	}
	top, err := walkTop(ctx, src, size, limits)
	if err != nil {
		return Layout{}, err
	}
	if len(top) == 0 || top[0].typ != "ftyp" {
		return Layout{}, invalid("the file does not start with an ftyp box")
	}
	moovAt, mdatAt := -1, -1
	for i, h := range top {
		switch h.typ {
		case "moov":
			if moovAt >= 0 {
				return Layout{}, invalid("a second moov box at %d", h.off)
			}
			moovAt = i
		case "mdat":
			if mdatAt < 0 {
				mdatAt = i
			}
		}
	}
	if moovAt < 0 {
		return Layout{}, invalid("no moov box")
	}
	if mdatAt < 0 || moovAt < mdatAt {
		return Layout{}, ErrAlreadyFaststart
	}
	for _, h := range top {
		if fragmentBoxes[h.typ] {
			return Layout{}, invalid("a fragmented file (a %s box at %d) with its moov after its media data", h.typ, h.off)
		}
		if h.typ == "meta" || h.typ == "meco" {
			// Item locations (iloc) in it may name offsets that move.
			return Layout{}, invalid("a top-level %s box at %d, whose item locations the rewrite does not move", h.typ, h.off)
		}
	}
	old := top[moovAt]
	if old.size > limits.MaxMoovBytes {
		return Layout{}, invalid("a moov box of %d bytes, more than %d", old.size, limits.MaxMoovBytes)
	}
	raw, err := readRange(ctx, src, old.off, old.size)
	if err != nil {
		return Layout{}, err
	}
	p := &moovParser{limits: limits}
	tree, err := p.container(raw, old)
	if err != nil {
		return Layout{}, err
	}
	if len(p.tables) == 0 {
		return Layout{}, invalid("the moov box names no chunk offsets to move")
	}
	m := mover{
		mediaFrom: top[mdatAt].off, mediaTo: old.off,
		afterFrom: old.off + old.size, afterTo: size,
		oldMoov: old.size,
	}
	upgrade := false
	m.newMoov = tree.size(false)
	for _, t := range p.tables {
		overflows, err := m.check(t)
		if err != nil {
			return Layout{}, err
		}
		upgrade = upgrade || overflows
	}
	if upgrade {
		m.newMoov = tree.size(true)
	}
	if m.newMoov > limits.MaxMoovBytes {
		return Layout{}, invalid("a rewritten moov box of %d bytes, more than %d", m.newMoov, limits.MaxMoovBytes)
	}
	moov := make([]byte, 0, m.newMoov)
	moov = tree.write(moov, upgrade, m)
	if int64(len(moov)) != m.newMoov {
		return Layout{}, fmt.Errorf("faststart: wrote a moov of %d bytes, planned %d", len(moov), m.newMoov)
	}
	layout := Layout{Size: size - old.size + m.newMoov, MoovAt: m.mediaFrom, moov: moov}
	for _, s := range []segment{
		{at: 0, n: m.mediaFrom, from: 0},
		{at: m.mediaFrom, n: m.newMoov, data: moov},
		{at: m.mediaFrom + m.newMoov, n: m.mediaTo - m.mediaFrom, from: m.mediaFrom},
		{at: m.afterFrom - m.oldMoov + m.newMoov, n: m.afterTo - m.afterFrom, from: m.afterFrom},
	} {
		if s.n > 0 {
			layout.segments = append(layout.segments, s)
		}
	}
	layout.probes = m.probes(p.tables)
	return layout, nil
}

// mover moves chunk offsets with the bytes they point into.
type mover struct {
	// mediaFrom and mediaTo bound the bytes from the first mdat to the old
	// moov: they move by the new moov's size.
	mediaFrom, mediaTo int64
	// afterFrom and afterTo bound the bytes after the old moov: they move
	// by its growth.
	afterFrom, afterTo int64
	oldMoov, newMoov   int64
}

// move is where the byte at off of the source is in the rewrite.
func (m mover) move(off int64) (int64, bool) {
	switch {
	case off >= m.mediaFrom && off < m.mediaTo:
		return off + m.newMoov, true
	case off >= m.afterFrom && off < m.afterTo:
		return off - m.oldMoov + m.newMoov, true
	}
	return 0, false
}

// end is where the run of the source that holds off ends.
func (m mover) end(off int64) int64 {
	if off < m.mediaTo {
		return m.mediaTo
	}
	return m.afterTo
}

// check refuses a table with an offset that points outside the bytes that
// move, and reports whether an stco would pass 4 GiB once moved.
func (m mover) check(t *offsetTable) (overflows bool, err error) {
	for i := 0; i < t.count; i++ {
		off := t.entry(i)
		moved, ok := m.move(off)
		if !ok {
			return false, invalid("chunk offset %d of a %s box points outside the media data", off, t.typ())
		}
		if !t.wide && moved > math.MaxUint32 {
			overflows = true
		}
	}
	return overflows, nil
}

// maxProbes bounds the chunks Verify compares.
const maxProbes = 16

// probes are the first and last chunk of each table, up to maxProbes of
// them, 16 bytes each (fewer where the run ends sooner).
func (m mover) probes(tables []*offsetTable) []probe {
	var out []probe
	for _, t := range tables {
		indexes := []int{0, t.count - 1}
		if t.count <= 1 {
			indexes = indexes[:t.count]
		}
		for _, i := range indexes {
			if len(out) == maxProbes {
				return out
			}
			from := t.entry(i)
			to, _ := m.move(from)
			out = append(out, probe{from: from, to: to, n: min(16, m.end(from)-from)})
		}
	}
	return out
}

// Read reads n bytes of the rewritten file from off: the rewritten moov
// from memory, the rest from src by ranged reads.
func (l Layout) Read(ctx context.Context, src Source, off, n int64) ([]byte, error) {
	if off < 0 || n < 0 || off+n > l.Size {
		return nil, fmt.Errorf("faststart: bytes %d+%d are outside the rewritten file of %d", off, n, l.Size)
	}
	out := make([]byte, n)
	for _, s := range l.segments {
		lo, hi := max(off, s.at), min(off+n, s.at+s.n)
		if lo >= hi {
			continue
		}
		dst := out[lo-off : hi-off]
		if s.data != nil {
			copy(dst, s.data[lo-s.at:hi-s.at])
			continue
		}
		if err := readInto(ctx, src, s.from+lo-s.at, dst); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// CopySource reports where in the source the n bytes of the rewritten file
// from off are, when they are one run of the source's bytes: storage can
// then copy them itself (an UploadPartCopy).
func (l Layout) CopySource(off, n int64) (from int64, ok bool) {
	for _, s := range l.segments {
		if s.data == nil && off >= s.at && off+n <= s.at+s.n {
			return s.from + off - s.at, true
		}
	}
	return 0, false
}

// Verify checks a stored rewrite (rewritten, of Size bytes) against the
// plan: its moov comes before its media data, is where the plan put it and
// holds exactly the rewritten moov, and the chunks the plan probes are the
// source's bytes. ErrMismatch when it is not; any other error is storage's.
func Verify(ctx context.Context, rewritten, source Source, l Layout, limits Limits) error {
	top, err := walkTop(ctx, rewritten, l.Size, limits)
	var refusal *Refusal
	if errors.As(err, &refusal) {
		return fmt.Errorf("%w: %s", ErrMismatch, refusal.Reason)
	}
	if err != nil {
		return err
	}
	moovAt, mdatAt := -1, -1
	for i, h := range top {
		if h.typ == "moov" && moovAt < 0 {
			moovAt = i
		}
		if h.typ == "mdat" && mdatAt < 0 {
			mdatAt = i
		}
	}
	if moovAt < 0 || (mdatAt >= 0 && mdatAt < moovAt) || top[moovAt].off != l.MoovAt || top[moovAt].size != int64(len(l.moov)) {
		return fmt.Errorf("%w: its moov box is not where the plan put it", ErrMismatch)
	}
	stored, err := readRange(ctx, rewritten, l.MoovAt, int64(len(l.moov)))
	if err != nil {
		return err
	}
	if !bytes.Equal(stored, l.moov) {
		return fmt.Errorf("%w: its moov box is not the rewritten one", ErrMismatch)
	}
	for _, p := range l.probes {
		want, err := readRange(ctx, source, p.from, p.n)
		if err != nil {
			return err
		}
		got, err := readRange(ctx, rewritten, p.to, p.n)
		if err != nil {
			return err
		}
		if !bytes.Equal(got, want) {
			return fmt.Errorf("%w: the chunk at %d is not the source's chunk at %d", ErrMismatch, p.to, p.from)
		}
	}
	return nil
}

// readRange reads the n bytes of src from off; n is bounded by the caller.
func readRange(ctx context.Context, src Source, off, n int64) ([]byte, error) {
	out := make([]byte, n)
	if err := readInto(ctx, src, off, out); err != nil {
		return nil, err
	}
	return out, nil
}

func readInto(ctx context.Context, src Source, off int64, dst []byte) error {
	if len(dst) == 0 {
		return nil
	}
	body, err := src.OpenRange(ctx, off, int64(len(dst)))
	if err != nil {
		return err
	}
	defer body.Close()
	if _, err := io.ReadFull(body, dst); err != nil {
		return fmt.Errorf("faststart: read bytes %d+%d: %w", off, len(dst), err)
	}
	return nil
}
