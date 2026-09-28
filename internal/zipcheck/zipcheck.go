// Package zipcheck decides, before core streams a ZIP to clamd, whether
// clamd would scan all of it (media redesign ticket 23). Checked against a
// real clamd 1.5.4, clamd skips without a report a member that inflates past
// its MaxFileSize, and it goes by what the member really inflates to, not by
// the sizes its headers declare. It also skips a member with ZIP64 sizes or
// a compression method it does not know. So a ZIP passes only when every
// member, read as clamd reads it, is within clamd's limits:
//
//   - its directory is read by bounded ranged reads (the end record, then the
//     central directory and what follows it, never more than
//     maxDirectoryBytes), and its counts and declared sizes are held to the
//     limits;
//   - the whole file is then streamed once, in order, and never kept: every
//     member's local header must agree with the directory, the members must
//     cover the file from its first byte to the directory with nothing
//     between or over them, and every member is inflated to prove it holds
//     exactly the bytes and checksum it declares;
//   - an archive inside it (by its name or its first bytes) passes only if it
//     is a ZIP one level deep that passes the same checks, its members
//     counting toward the same limits (Limits.nesting).
//
// See "Malware scan" in docs/media-lifecycle.md.
package zipcheck

import (
	"bufio"
	"bytes"
	"cmp"
	"compress/flate"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"slices"
	"strings"
)

var (
	// ErrTooLarge refuses a ZIP that holds more than clamd scans whole:
	// clamd would skip part of it without a report.
	ErrTooLarge = errors.New("zipcheck: the ZIP holds more than clamd scans whole")
	// ErrInvalid refuses a ZIP that is malformed, or whose members clamd
	// cannot read.
	ErrInvalid = errors.New("zipcheck: the ZIP is malformed or holds what clamd cannot read")
)

// Refusal is a ZIP the check refuses, and why. Reason names members by
// their place in the directory, never by name. errors.Is matches Err,
// ErrTooLarge or ErrInvalid.
type Refusal struct {
	Err    error
	Reason string
}

func (r *Refusal) Error() string { return r.Err.Error() + ": " + r.Reason }

func (r *Refusal) Unwrap() error { return r.Err }

func tooLarge(format string, args ...any) error {
	return &Refusal{Err: ErrTooLarge, Reason: fmt.Sprintf(format, args...)}
}

func invalid(format string, args ...any) error {
	return &Refusal{Err: ErrInvalid, Reason: fmt.Sprintf(format, args...)}
}

// MaxEntries is the most members the check takes in one archive, whatever
// the limits say: the end record counts them in 16 bits, and ZIP64 counts
// are refused.
const MaxEntries = 1<<16 - 1

// Limits are clamd's archive limits (clamd.conf), which a ZIP is held
// within.
type Limits struct {
	// MaxFileSize is the most one member may inflate to, in bytes.
	MaxFileSize int64
	// MaxScanSize is the most all members together may inflate to.
	MaxScanSize int64
	// MaxFiles is the most members, at every level together; at most
	// MaxEntries.
	MaxFiles int
	// MaxRecursion is clamd's MaxRecursion: a file inside n archives is
	// scanned only while n < MaxRecursion (checked against clamd 1.5.4).
	// At least 2, so that clamd reaches the members of the ZIP itself.
	MaxRecursion int
}

// Validate refuses limits the check cannot hold a ZIP within.
func (l Limits) Validate() error {
	switch {
	case l.MaxFileSize < 1 || l.MaxScanSize < 1:
		return errors.New("zipcheck: MaxFileSize and MaxScanSize must be positive")
	case l.MaxFiles < 1 || l.MaxFiles > MaxEntries:
		return fmt.Errorf("zipcheck: MaxFiles must be from 1 to %d", MaxEntries)
	case l.MaxRecursion < 2:
		return errors.New("zipcheck: MaxRecursion must be at least 2, or clamd reaches no member of the ZIP")
	}
	return nil
}

// nesting is how many archives deep inside the ZIP an archive may sit: one
// (an archive among its members), or none while clamd's MaxRecursion would
// not reach the members of that archive (a member of a member is inside
// two archives, which clamd scans only while 2 < MaxRecursion).
func (l Limits) nesting() int {
	return min(1, l.MaxRecursion-2)
}

// Source is a stored file read by byte ranges (ranged GETs).
type Source interface {
	// OpenRange streams the n bytes of the file from off.
	OpenRange(ctx context.Context, off, n int64) (io.ReadCloser, error)
}

const (
	endSig        = 0x06054b50
	locatorSig    = 0x07064b50
	end64Sig      = 0x06064b50
	dirSig        = 0x02014b50
	localSig      = 0x04034b50
	descriptorSig = 0x08074b50

	endLen     = 22
	maxComment = 1<<16 - 1
	locatorLen = 20
	end64Len   = 56
	dirLen     = 46
	localLen   = 30
	// descriptorLen is the shortest data descriptor: no signature, 32-bit
	// sizes.
	descriptorLen = 12

	// flagsEncrypted are the flags of an encrypted member: encrypted,
	// strongly encrypted, and the directory's values masked.
	flagsEncrypted = 1<<0 | 1<<6 | 1<<13
	flagDescriptor = 1 << 3

	methodStore   = 0
	methodDeflate = 8

	// maxNameBytes bounds a member's name.
	maxNameBytes = 1024
	// maxReadBytes bounds one ranged read of the directory.
	maxReadBytes = 1 << 20
	// maxDirectoryBytes bounds what the check holds of an archive's end: the
	// central directory and the records after it. A nested archive's end
	// is kept from its inflated bytes, at most this much of it.
	maxDirectoryBytes = 8 << 20
	// headBytes is how much of a member's start is kept to tell an archive
	// by its first bytes (ISO 9660's mark is at 32769).
	headBytes = 32774
)

// Check reads the ZIP of size bytes that src holds and returns nil when
// clamd would scan all of it, a *Refusal when it would not, or the error
// that stopped the check (storage, ctx), which says nothing about the ZIP.
//
// It reads the directory in ranges of at most 1 MiB, then streams the file
// once from its first byte, and each ZIP inside it once more (its own data,
// inflated again). Memory stays bounded: the directory, and the last
// maxDirectoryBytes of a member that may be a nested ZIP.
func Check(ctx context.Context, src Source, size int64, limits Limits) error {
	if err := limits.Validate(); err != nil {
		return err
	}
	b := &budget{limits: limits}
	dir, err := readDirectory(ctx, sourceRanges{src}, size, "", 0, b)
	if err != nil || len(dir.entries) == 0 {
		return err
	}
	body, err := src.OpenRange(ctx, 0, size)
	if err != nil {
		return err
	}
	w := newWalker(ctx, body, size, "", 0, b, dir)
	err = w.walk(dir)
	body.Close()
	if err != nil {
		return err
	}
	for _, n := range w.nested {
		if err := checkNested(ctx, src, n, b); err != nil {
			return err
		}
	}
	return nil
}

// nested is a ZIP inside the ZIP, found by the walk: its directory, read
// from its last bytes, and where its data lies in the file.
type nested struct {
	at     place
	offset int64
	method uint16
	csize  int64
	usize  int64
	dir    *directory
}

// checkNested walks a ZIP inside the ZIP, reading its data from the file
// again and inflating it as the walk of the ZIP did.
func checkNested(ctx context.Context, src Source, n nested, b *budget) error {
	if len(n.dir.entries) == 0 {
		return nil
	}
	body, err := src.OpenRange(ctx, n.offset, n.csize)
	if err != nil {
		return err
	}
	defer body.Close()
	var data io.Reader = body
	if n.method == methodDeflate {
		inflated := flate.NewReader(bufio.NewReader(body))
		defer inflated.Close()
		data = inflated
	}
	return newWalker(ctx, data, n.usize, n.at, 1, b, n.dir).walk(n.dir)
}

// place is where an archive sits: "" for the ZIP itself, "member 5" for a
// ZIP among its members.
type place string

func (p place) archive() string {
	if p == "" {
		return "the ZIP"
	}
	return string(p)
}

func (p place) member(k int) string {
	if p == "" {
		return fmt.Sprintf("member %d", k)
	}
	return fmt.Sprintf("member %d of %s", k, p)
}

// ranges reads byte ranges of an archive.
type ranges interface {
	read(ctx context.Context, off, n int64) ([]byte, error)
}

// sourceRanges reads the stored file, at most maxReadBytes a ranged read.
type sourceRanges struct{ src Source }

func (s sourceRanges) read(ctx context.Context, off, n int64) ([]byte, error) {
	out := make([]byte, n)
	for done := int64(0); done < n; {
		step := min(n-done, maxReadBytes)
		body, err := s.src.OpenRange(ctx, off+done, step)
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

// tailRanges reads the end of a nested ZIP, kept as it was inflated: the
// bytes from base to its end.
type tailRanges struct {
	at   place
	tail []byte
	base int64
}

func (t tailRanges) read(_ context.Context, off, n int64) ([]byte, error) {
	if off < t.base {
		return nil, tooLarge("%s's directory is larger than core reads (%d MiB)", t.at.archive(), maxDirectoryBytes>>20)
	}
	return t.tail[off-t.base : off-t.base+n], nil
}

// budget is what the members of the ZIP, and of the ZIPs inside it, add up
// to against the limits.
type budget struct {
	limits Limits
	files  int
	bytes  int64
}

// count takes n members.
func (b *budget) count(n int) error {
	b.files += n
	if b.files > b.limits.MaxFiles {
		return tooLarge("%d members in all, more than MaxFiles (%d)", b.files, b.limits.MaxFiles)
	}
	return nil
}

// add takes a member that declares it inflates to usize bytes.
func (b *budget) add(member string, usize int64) error {
	if usize > b.limits.MaxFileSize {
		return tooLarge("%s inflates to %d bytes, more than MaxFileSize (%d)", member, usize, b.limits.MaxFileSize)
	}
	b.bytes += usize
	if b.bytes > b.limits.MaxScanSize {
		return tooLarge("the members inflate to more than MaxScanSize (%d) in all", b.limits.MaxScanSize)
	}
	return nil
}

// entry is one member as the central directory lists it.
type entry struct {
	// index is the member's place in the directory, from 1.
	index  int
	name   string
	flags  uint16
	method uint16
	crc    uint32
	csize  int64
	usize  int64
	offset int64
}

// directory is an archive as its central directory describes it.
type directory struct {
	at   place
	size int64
	// entries are by offset.
	entries []entry
	// start is where the central directory starts: the members end there.
	start int64
	// endSum is the SHA-256 of everything from start to the end of the
	// archive, as the directory was read.
	endSum [32]byte
}

// readDirectory reads the end record and the central directory of the
// archive of size bytes at, depth archives deep, and takes its members
// from the budget.
func readDirectory(ctx context.Context, r ranges, size int64, at place, depth int, b *budget) (*directory, error) {
	name := at.archive()
	if size < endLen {
		return nil, invalid("%s is shorter than an end record", name)
	}
	tailLen := min(size, endLen+maxComment+locatorLen)
	tail, err := r.read(ctx, size-tailLen, tailLen)
	if err != nil {
		return nil, err
	}
	// The end record is the one whose comment ends exactly at the end of
	// the file; two such records make the archive read two ways.
	found := -1
	for i := len(tail) - endLen; i >= 0; i-- {
		if le32(tail[i:]) == endSig && int(le16(tail[i+20:])) == len(tail)-i-endLen {
			if found >= 0 {
				return nil, invalid("%s has two end records", name)
			}
			found = i
		}
	}
	if found < 0 {
		return nil, invalid("%s has no end record at its end", name)
	}
	end := tail[found:]
	endPos := size - tailLen + int64(found)
	disk, dirDisk, here, count := le16(end[4:]), le16(end[6:]), le16(end[8:]), int(le16(end[10:]))
	dirSize, dirOffset := int64(le32(end[12:])), int64(le32(end[16:]))
	if disk == 0xffff || dirDisk == 0xffff || here == 0xffff || count == 0xffff || dirSize == 0xffffffff || dirOffset == 0xffffffff {
		return nil, invalid("%s leaves its counts or offsets to ZIP64, which clamd does not read", name)
	}
	if disk != 0 || dirDisk != 0 || int(here) != count {
		return nil, invalid("%s spans disks", name)
	}
	dirEnd := endPos
	if found >= locatorLen && le32(tail[found-locatorLen:]) == locatorSig {
		if dirEnd, err = readZip64End(ctx, r, name, tail[found-locatorLen:found], endPos-locatorLen, count, dirSize, dirOffset); err != nil {
			return nil, err
		}
	}
	if dirOffset+dirSize != dirEnd {
		return nil, invalid("%s's directory is not where its end record says", name)
	}
	if err := b.count(count); err != nil {
		return nil, err
	}
	if size-dirOffset > maxDirectoryBytes {
		return nil, tooLarge("%s's directory is larger than core reads (%d MiB)", name, maxDirectoryBytes>>20)
	}
	region, err := r.read(ctx, dirOffset, size-dirOffset)
	if err != nil {
		return nil, err
	}
	d := &directory{at: at, size: size, start: dirOffset, endSum: sha256.Sum256(region)}
	cd := region[:dirSize]
	p := 0
	for k := 1; k <= count; k++ {
		if len(cd)-p < dirLen || le32(cd[p:]) != dirSig {
			return nil, invalid("%s's directory ends before its %d members", name, count)
		}
		h := cd[p:]
		e := entry{
			index: k, flags: le16(h[8:]), method: le16(h[10:]), crc: le32(h[16:]),
			csize: int64(le32(h[20:])), usize: int64(le32(h[24:])), offset: int64(le32(h[42:])),
		}
		nameLen := int(le16(h[28:]))
		next := p + dirLen + nameLen + int(le16(h[30:])) + int(le16(h[32:]))
		if next > len(cd) {
			return nil, invalid("%s's directory entry runs past the directory", at.member(k))
		}
		e.name = string(h[dirLen : dirLen+nameLen])
		p = next
		if e.csize == 0xffffffff || e.usize == 0xffffffff || e.offset == 0xffffffff || le16(h[34:]) == 0xffff {
			return nil, invalid("%s has ZIP64 sizes, which clamd does not read", at.member(k))
		}
		if le16(h[34:]) != 0 {
			return nil, invalid("%s starts on another disk", at.member(k))
		}
		if err := e.readable(at); err != nil {
			return nil, err
		}
		if isArchiveName(e.name) && depth+1 > b.limits.nesting() {
			return nil, nestedTooDeep(at.member(k))
		}
		if err := b.add(at.member(k), e.usize); err != nil {
			return nil, err
		}
		d.entries = append(d.entries, e)
	}
	if p != len(cd) {
		return nil, invalid("%s's directory holds more than its %d members", name, count)
	}
	return d, d.checkLayout()
}

func nestedTooDeep(member string) error {
	return tooLarge("%s is an archive nested deeper than core reads", member)
}

// readZip64End reads the ZIP64 end record the locator before the end record
// points at, which must sit right before the locator and repeat the end
// record's counts, directory size and offset. It returns where it starts:
// the directory ends there.
func readZip64End(ctx context.Context, r ranges, name string, locator []byte, locatorPos int64, count int, dirSize, dirOffset int64) (int64, error) {
	if le32(locator[4:]) != 0 || le32(locator[16:]) != 1 {
		return 0, invalid("%s's ZIP64 end record is on another disk", name)
	}
	at := le64(locator[8:])
	if locatorPos < end64Len || at > uint64(locatorPos-end64Len) {
		return 0, invalid("%s's ZIP64 end record is not where its locator says", name)
	}
	pos := int64(at)
	record, err := r.read(ctx, pos, end64Len)
	if err != nil {
		return 0, err
	}
	if le32(record) != end64Sig || le64(record[4:]) != uint64(locatorPos-pos-12) {
		return 0, invalid("%s's ZIP64 end record is not where its locator says", name)
	}
	if le32(record[16:]) != 0 || le32(record[20:]) != 0 || le64(record[24:]) != uint64(count) || le64(record[32:]) != uint64(count) ||
		le64(record[40:]) != uint64(dirSize) || le64(record[48:]) != uint64(dirOffset) {
		return 0, invalid("%s's ZIP64 end record disagrees with its end record", name)
	}
	return pos, nil
}

// readable refuses a member clamd cannot read, or whose name is not one.
func (e entry) readable(at place) error {
	m := at.member(e.index)
	switch {
	case e.flags&flagsEncrypted != 0:
		return invalid("%s is encrypted", m)
	case e.method != methodStore && e.method != methodDeflate:
		return invalid("%s is compressed with method %d; only stored and deflated members are read", m, e.method)
	case e.method == methodStore && e.csize != e.usize:
		return invalid("%s is stored, but its sizes differ", m)
	case e.name == "" || len(e.name) > maxNameBytes || strings.IndexByte(e.name, 0) >= 0:
		return invalid("%s's name is empty, longer than %d bytes or holds a NUL", m, maxNameBytes)
	}
	return nil
}

// checkLayout checks, from the directory alone, that the members can lie
// one after the other from the archive's first byte to its directory: each
// needs at least its local header, its name, its data and, when it has one,
// the shortest data descriptor before the next one starts. The walk then
// checks that they do, exactly.
func (d *directory) checkLayout() error {
	slices.SortStableFunc(d.entries, func(a, b entry) int { return cmp.Compare(a.offset, b.offset) })
	name := d.at.archive()
	if len(d.entries) == 0 {
		if d.start != 0 {
			return invalid("bytes before %s's empty directory belong to no member", name)
		}
		return nil
	}
	if d.entries[0].offset != 0 {
		return invalid("bytes before %s, the first, belong to no member", d.at.member(d.entries[0].index))
	}
	for i, e := range d.entries {
		if e.offset >= d.start {
			return invalid("%s points past the members, into the directory or beyond the file", d.at.member(e.index))
		}
		least := e.offset + localLen + int64(len(e.name)) + e.csize
		if e.flags&flagDescriptor != 0 {
			least += descriptorLen
		}
		if i+1 < len(d.entries) && least > d.entries[i+1].offset {
			return invalid("%s and %s overlap", d.at.member(e.index), d.at.member(d.entries[i+1].index))
		}
		if least > d.start {
			return invalid("%s runs into the directory", d.at.member(e.index))
		}
	}
	return nil
}

// walker reads an archive in order, from its first byte to its last.
type walker struct {
	ctx   context.Context
	s     *stream
	at    place
	depth int
	b     *budget
	copy  []byte
	// tail keeps the last bytes of each member, where a ZIP inside it
	// keeps its directory; nil where no archive may nest.
	tail   *ring
	nested []nested
}

func newWalker(ctx context.Context, r io.Reader, size int64, at place, depth int, b *budget, dir *directory) *walker {
	w := &walker{
		ctx: ctx, s: &stream{r: bufio.NewReaderSize(r, 64<<10), size: size},
		at: at, depth: depth, b: b, copy: make([]byte, 32<<10),
	}
	if depth < b.limits.nesting() {
		var largest int64
		for _, e := range dir.entries {
			largest = max(largest, e.usize)
		}
		w.tail = &ring{buf: make([]byte, min(largest, maxDirectoryBytes))}
	}
	return w
}

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

// member checks one member: its local header, its data, and what follows it
// up to next, where the next member (or the directory) starts.
func (w *walker) member(e entry, next int64) error {
	if err := w.ctx.Err(); err != nil {
		return err
	}
	m := w.at.member(e.index)
	if w.s.pos != e.offset {
		return invalid("bytes before %s belong to no member", m)
	}
	h, err := w.s.next(localLen)
	if err != nil {
		return err
	}
	if le32(h) != localSig {
		return invalid("%s has no local header where the directory says", m)
	}
	nameLen, extraLen := int(le16(h[26:])), int64(le16(h[28:]))
	if (le16(h[6:])^e.flags)&(flagsEncrypted|flagDescriptor) != 0 || le16(h[8:]) != e.method || nameLen != len(e.name) {
		return invalid("%s's local header disagrees with the directory", m)
	}
	name, err := w.s.next(nameLen)
	if err != nil {
		return err
	}
	if string(name) != e.name {
		return invalid("%s's local header disagrees with the directory", m)
	}
	// Without a data descriptor, the local header gives the member's
	// checksum and sizes; with one, it gives them or leaves them zero.
	crc, csize, usize := le32(h[14:]), le32(h[18:]), le32(h[22:])
	if csize == 0xffffffff || usize == 0xffffffff {
		return invalid("%s has ZIP64 sizes, which clamd does not read", m)
	}
	given := func(v uint32, want int64) bool { return int64(v) == want || (e.flags&flagDescriptor != 0 && v == 0) }
	if !given(crc, int64(e.crc)) || !given(csize, e.csize) || !given(usize, e.usize) {
		return invalid("%s's local header disagrees with the directory", m)
	}
	if err := w.s.skip(extraLen); err != nil {
		return err
	}
	offset := w.s.pos
	if offset+e.csize > next {
		return invalid("%s overlaps what follows it", m)
	}
	out := &sink{ctx: w.ctx, member: m, declared: e.usize, tail: w.tail}
	if err := w.data(e, m, out); err != nil {
		return err
	}
	if out.n != e.usize || out.crc != e.crc {
		return invalid("%s inflates to other bytes than it declares", m)
	}
	if e.flags&flagDescriptor != 0 {
		if err := w.descriptor(e, m, next); err != nil {
			return err
		}
	}
	if w.s.pos != next {
		return invalid("bytes after %s belong to no member", m)
	}
	if e.usize > 0 && (isArchiveName(e.name) || isArchive(out.head)) {
		return w.archive(e, m, offset, out.head)
	}
	return nil
}

// data streams the member's data through out, inflating a deflated one.
func (w *walker) data(e entry, m string, out *sink) error {
	data := &section{s: w.s, left: e.csize}
	if w.tail != nil {
		w.tail.reset()
	}
	if e.method == methodStore {
		_, err := io.CopyBuffer(out, data, w.copy)
		return err
	}
	inflated := flate.NewReader(data)
	defer inflated.Close()
	_, err := io.CopyBuffer(out, inflated, w.copy)
	var corrupt flate.CorruptInputError
	switch {
	case errors.Is(err, io.ErrUnexpectedEOF):
		return invalid("%s's deflate data ends before its stream does", m)
	case errors.As(err, &corrupt):
		return invalid("%s's deflate data is corrupt", m)
	case err != nil:
		return err
	case data.left != 0:
		return invalid("%s has bytes after its deflate stream", m)
	}
	return nil
}

// archive takes a member that is an archive: a ZIP one level deep, whose
// directory is read from the member's last bytes and whose members the
// budget takes now; anything else is refused.
func (w *walker) archive(e entry, m string, offset int64, head []byte) error {
	if w.depth+1 > w.b.limits.nesting() {
		return nestedTooDeep(m)
	}
	if !isZip(head) {
		return tooLarge("%s is an archive core cannot read inside", m)
	}
	at := place(m)
	tail := w.tail.last()
	dir, err := readDirectory(w.ctx, tailRanges{at: at, tail: tail, base: e.usize - int64(len(tail))}, e.usize, at, w.depth+1, w.b)
	if err != nil {
		return err
	}
	w.nested = append(w.nested, nested{at: at, offset: offset, method: e.method, csize: e.csize, usize: e.usize, dir: dir})
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

// sink takes a member's inflated bytes, never more than it declares: it
// sums their checksum, keeps their first headBytes and, where an archive
// may nest, their last ones.
type sink struct {
	ctx      context.Context
	member   string
	declared int64
	n        int64
	crc      uint32
	head     []byte
	tail     *ring
}

func (k *sink) Write(p []byte) (int, error) {
	if err := k.ctx.Err(); err != nil {
		return 0, err
	}
	if int64(len(p)) > k.declared-k.n {
		return 0, invalid("%s inflates to more than the %d bytes it declares", k.member, k.declared)
	}
	k.n += int64(len(p))
	k.crc = crc32.Update(k.crc, crc32.IEEETable, p)
	if room := headBytes - len(k.head); room > 0 {
		k.head = append(k.head, p[:min(room, len(p))]...)
	}
	if k.tail != nil {
		k.tail.write(p)
	}
	return len(p), nil
}

// ring keeps the last len(buf) bytes written to it.
type ring struct {
	buf []byte
	// n is how many bytes were written since the last reset.
	n int64
}

func (r *ring) reset() { r.n = 0 }

func (r *ring) write(p []byte) {
	size := len(r.buf)
	if size == 0 {
		return
	}
	if len(p) > size {
		r.n += int64(len(p) - size)
		p = p[len(p)-size:]
	}
	for len(p) > 0 {
		at := int(r.n % int64(size))
		c := copy(r.buf[at:], p)
		r.n += int64(c)
		p = p[c:]
	}
}

// last is what the ring keeps, oldest first. It reorders the ring in place.
func (r *ring) last() []byte {
	size := int64(len(r.buf))
	if r.n <= size {
		return r.buf[:r.n]
	}
	at := int(r.n % size)
	slices.Reverse(r.buf[:at])
	slices.Reverse(r.buf[at:])
	slices.Reverse(r.buf)
	r.n = size
	return r.buf
}

// archiveExtensions name archive formats clamd reads inside, by the
// lowercased extension of a member's name.
var archiveExtensions = map[string]bool{
	"zip": true, "jar": true, "war": true, "ear": true, "apk": true, "aar": true, "7z": true, "rar": true,
	"tar": true, "gz": true, "tgz": true, "bz2": true, "tbz": true, "tbz2": true, "xz": true, "txz": true,
	"lzma": true, "cab": true, "arj": true, "lzh": true, "lha": true, "cpio": true, "iso": true, "img": true,
	"dmg": true, "xar": true, "pkg": true, "egg": true, "alz": true,
}

// isArchiveName reports whether a member's name is an archive's.
func isArchiveName(name string) bool {
	if strings.HasSuffix(name, "/") {
		return false
	}
	base := name[strings.LastIndexAny(name, `/\`)+1:]
	dot := strings.LastIndexByte(base, '.')
	return dot >= 0 && archiveExtensions[strings.ToLower(base[dot+1:])]
}

// isZip reports whether a member starts as a ZIP does (Office documents,
// JARs and APKs do too).
func isZip(head []byte) bool {
	return bytes.HasPrefix(head, []byte("PK\x03\x04")) || bytes.HasPrefix(head, []byte("PK\x05\x06")) ||
		bytes.HasPrefix(head, []byte("PK\x07\x08"))
}

// archiveMagic are the first bytes of archive formats clamd reads inside,
// long enough that a file which only starts like one is not taken for it.
var archiveMagic = [][]byte{
	[]byte("7z\xbc\xaf\x27\x1c"),   // 7-Zip
	[]byte("Rar!\x1a\x07"),         // RAR 4 and 5
	{0x1f, 0x8b, 0x08},             // gzip (deflate, its only method)
	[]byte("\xfd7zXZ\x00"),         // xz
	[]byte("MSCF\x00\x00\x00\x00"), // Microsoft Cabinet
	[]byte("070701"),               // cpio, new ASCII
	[]byte("070702"),               // cpio, new ASCII with checksums
	[]byte("070707"),               // cpio, old ASCII
	[]byte("xar!"),                 // XAR
	[]byte("EGGA"),                 // ESTsoft EGG
	[]byte("ALZ\x01"),              // ALZip
}

// isArchive reports whether a member's first bytes are an archive's.
func isArchive(head []byte) bool {
	if isZip(head) {
		return true
	}
	for _, magic := range archiveMagic {
		if bytes.HasPrefix(head, magic) {
			return true
		}
	}
	switch {
	case len(head) >= 262 && string(head[257:262]) == "ustar": // tar
		return true
	case len(head) >= 4 && head[0] == 0x60 && head[1] == 0xea && le16(head[2:]) > 0 && le16(head[2:]) <= 2600: // ARJ
		return true
	case isBzip2(head):
		return true
	case len(head) >= 7 && string(head[2:4]) == "-l" && (head[4] == 'h' || head[4] == 'z') && head[6] == '-': // LHA
		return true
	case len(head) >= headBytes && string(head[32769:32774]) == "CD001": // ISO 9660
		return true
	}
	return false
}

// isBzip2 reports whether a member starts as a bzip2 stream: "BZh", a block
// size from 1 to 9, then a block's magic or the end of an empty stream.
func isBzip2(head []byte) bool {
	return len(head) >= 10 && string(head[:3]) == "BZh" && head[3] >= '1' && head[3] <= '9' &&
		(string(head[4:10]) == "1AY&SY" || string(head[4:10]) == "\x17\x72\x45\x38\x50\x90")
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

func le16(b []byte) uint16 { return binary.LittleEndian.Uint16(b) }
func le32(b []byte) uint32 { return binary.LittleEndian.Uint32(b) }
func le64(b []byte) uint64 { return binary.LittleEndian.Uint64(b) }
