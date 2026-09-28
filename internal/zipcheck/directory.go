package zipcheck

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"fmt"
	"slices"
	"strings"
)

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
	// maxDirectoryBytes bounds what the check holds of an archive's end: the
	// central directory and the records after it.
	maxDirectoryBytes = 8 << 20
)

// place is where an archive sits, for a refusal's reason: "" for the ZIP
// itself, "member 5" for a ZIP among its members, and so on inward.
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
// archive of size bytes that src holds, at, depth archives deep, and takes
// its members from the budget. The archive's offsets count from its own
// first byte or, for a ZIP embedded past the first byte of a file (stub > 0,
// a self-extractor as zip -A leaves it), from that file's first byte.
func readDirectory(ctx context.Context, src Source, size int64, at place, depth int, b *budget, stub int64) (*directory, error) {
	name := at.archive()
	if size < endLen {
		return nil, invalid("%s is shorter than an end record", name)
	}
	tailLen := min(size, endLen+maxComment+locatorLen)
	tail, err := readAt(ctx, src, size-tailLen, tailLen)
	if err != nil {
		return nil, err
	}
	// The end record is the one whose comment ends exactly at the end of
	// the file; no other end record signature may follow it (checked with
	// the directory below).
	found := -1
	for i := len(tail) - endLen; i >= 0; i-- {
		if le32(tail[i:]) == endSig && int(le16(tail[i+20:])) == len(tail)-i-endLen {
			found = i
			break
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
		if dirEnd, err = readZip64End(ctx, src, name, tail[found-locatorLen:found], endPos-locatorLen, count, dirSize, dirOffset, stub); err != nil {
			return nil, err
		}
	}
	base := int64(0)
	if dirOffset+dirSize != dirEnd && stub > 0 && dirOffset-stub+dirSize == dirEnd {
		base = stub
	}
	dirOffset -= base
	if dirOffset < 0 || dirOffset+dirSize != dirEnd {
		return nil, invalid("%s's directory is not where its end record says", name)
	}
	if err := b.count(count); err != nil {
		return nil, err
	}
	if size-dirOffset > maxDirectoryBytes {
		return nil, tooLarge("%s's directory is larger than core reads (%d MiB)", name, maxDirectoryBytes>>20)
	}
	region, err := readAt(ctx, src, dirOffset, size-dirOffset)
	if err != nil {
		return nil, err
	}
	// The directory and what follows it hold no end record signature but
	// the end record's own: a reader taking another for the end would read
	// another archive. (The walk checks the members' headers.)
	if i := strayEndRecord(region, endPos-dirOffset); i >= 0 {
		return nil, invalid("%s holds an end record signature at byte %d, outside its members' data", name, dirOffset+int64(i))
	}
	d := &directory{size: size, start: dirOffset, endSum: sha256.Sum256(region)}
	cd := region[:dirSize]
	p := 0
	for k := 1; k <= count; k++ {
		if len(cd)-p < dirLen || le32(cd[p:]) != dirSig {
			return nil, invalid("%s's directory ends before its %d members", name, count)
		}
		e, next, err := readEntry(cd, p, k, at, base)
		if err != nil {
			return nil, err
		}
		p = next
		if err := b.add(at.member(k), e.usize); err != nil {
			return nil, err
		}
		d.entries = append(d.entries, e)
	}
	if p != len(cd) {
		return nil, invalid("%s's directory holds more than its %d members", name, count)
	}
	return d, d.checkLayout(at)
}

// endRecordMark is the end record's signature.
var endRecordMark = []byte("PK\x05\x06")

// strayEndRecord is where in b an end record signature starts, other than at
// own (-1 for none there), or -1 when there is none.
func strayEndRecord(b []byte, own int64) int {
	for from := 0; ; {
		i := bytes.Index(b[from:], endRecordMark)
		if i < 0 {
			return -1
		}
		if i += from; int64(i) != own {
			return i
		}
		from = i + 1
	}
}

// readEntry reads member k's directory entry at p in the central directory
// cd, and where the next one starts.
func readEntry(cd []byte, p, k int, at place, base int64) (entry, int, error) {
	h := cd[p:]
	e := entry{
		index: k, flags: le16(h[8:]), method: le16(h[10:]), crc: le32(h[16:]),
		csize: int64(le32(h[20:])), usize: int64(le32(h[24:])), offset: int64(le32(h[42:])),
	}
	nameLen := int(le16(h[28:]))
	next := p + dirLen + nameLen + int(le16(h[30:])) + int(le16(h[32:]))
	m := at.member(k)
	if next > len(cd) {
		return entry{}, 0, invalid("%s's directory entry runs past the directory", m)
	}
	e.name = string(h[dirLen : dirLen+nameLen])
	if e.csize == 0xffffffff || e.usize == 0xffffffff || e.offset == 0xffffffff || le16(h[34:]) == 0xffff {
		return entry{}, 0, invalid("%s has ZIP64 sizes, which clamd does not read", m)
	}
	if le16(h[34:]) != 0 {
		return entry{}, 0, invalid("%s starts on another disk", m)
	}
	if e.offset -= base; e.offset < 0 {
		return entry{}, 0, invalid("%s points before the ZIP", m)
	}
	return e, next, e.readable(m)
}

// readZip64End reads the ZIP64 end record the locator before the end record
// points at, which must sit right before the locator and repeat the end
// record's counts, directory size and offset. The locator's offset counts as
// the archive's others do (from the archive, or from the stub before it).
// It returns where the record starts: the directory ends there.
func readZip64End(ctx context.Context, src Source, name string, locator []byte, locatorPos int64, count int, dirSize, dirOffset, stub int64) (int64, error) {
	if le32(locator[4:]) != 0 || le32(locator[16:]) != 1 {
		return 0, invalid("%s's ZIP64 end record is on another disk", name)
	}
	var record []byte
	pos := int64(-1)
	for i, shift := range []int64{0, stub} {
		if i > 0 && stub == 0 {
			break
		}
		at := int64(le64(locator[8:])) - shift
		if at < 0 || locatorPos < end64Len || at > locatorPos-end64Len {
			continue
		}
		r, err := readAt(ctx, src, at, end64Len)
		if err != nil {
			return 0, err
		}
		if le32(r) == end64Sig && le64(r[4:]) == uint64(locatorPos-at-12) {
			record, pos = r, at
			break
		}
	}
	if pos < 0 {
		return 0, invalid("%s's ZIP64 end record is not where its locator says", name)
	}
	if le32(record[16:]) != 0 || le32(record[20:]) != 0 || le64(record[24:]) != uint64(count) || le64(record[32:]) != uint64(count) ||
		le64(record[40:]) != uint64(dirSize) || le64(record[48:]) != uint64(dirOffset) {
		return 0, invalid("%s's ZIP64 end record disagrees with its end record", name)
	}
	return pos, nil
}

// readable refuses a member clamd cannot read, or whose name is not one.
func (e entry) readable(m string) error {
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
func (d *directory) checkLayout(at place) error {
	slices.SortStableFunc(d.entries, func(a, b entry) int { return cmp.Compare(a.offset, b.offset) })
	if len(d.entries) == 0 {
		if d.start != 0 {
			return invalid("bytes before %s's empty directory belong to no member", at.archive())
		}
		return nil
	}
	if d.entries[0].offset != 0 {
		return invalid("bytes before %s, the first, belong to no member", at.member(d.entries[0].index))
	}
	for i, e := range d.entries {
		if e.offset >= d.start {
			return invalid("%s points past the members, into the directory or beyond the file", at.member(e.index))
		}
		least := e.offset + localLen + int64(len(e.name)) + e.csize
		if e.flags&flagDescriptor != 0 {
			least += descriptorLen
		}
		if i+1 < len(d.entries) && least > d.entries[i+1].offset {
			return invalid("%s and %s overlap", at.member(e.index), at.member(d.entries[i+1].index))
		}
		if least > d.start {
			return invalid("%s runs into the directory", at.member(e.index))
		}
	}
	return nil
}
