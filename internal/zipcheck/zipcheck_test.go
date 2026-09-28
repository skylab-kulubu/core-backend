package zipcheck_test

import (
	"archive/zip"
	"bytes"
	"compress/flate"
	"compress/gzip"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"testing"

	"github.com/skylab-kulubu/core-backend/internal/zipcheck"
)

// source is a stored file in memory, read by ranges as the scan worker
// reads an object in the bucket. It records every range it serves.
type source struct {
	data  []byte
	reads []span
	fail  error
}

type span struct{ off, n int64 }

func (s *source) OpenRange(_ context.Context, off, n int64) (io.ReadCloser, error) {
	if s.fail != nil {
		return nil, s.fail
	}
	if off < 0 || n <= 0 || off+n > int64(len(s.data)) {
		return nil, fmt.Errorf("range %d+%d outside the %d bytes stored", off, n, len(s.data))
	}
	s.reads = append(s.reads, span{off, n})
	return io.NopCloser(bytes.NewReader(s.data[off : off+n])), nil
}

// largest is the longest range the check asked for.
func (s *source) largest() int64 {
	var most int64
	for _, r := range s.reads {
		most = max(most, r.n)
	}
	return most
}

// limits are clamd's limits scaled down so tests stay small: 1 MiB a
// member, 4 MiB in all, 100 members, and clamd's default recursion.
var limits = zipcheck.Limits{MaxFileSize: 1 << 20, MaxScanSize: 4 << 20, MaxFiles: 100, MaxRecursion: 17}

func check(t *testing.T, data []byte, l zipcheck.Limits) (error, *source) {
	t.Helper()
	src := &source{data: data}
	return zipcheck.Check(context.Background(), src, int64(len(data)), l), src
}

func passes(t *testing.T, data []byte, l zipcheck.Limits) {
	t.Helper()
	if err, _ := check(t, data, l); err != nil {
		t.Fatalf("refused: %v", err)
	}
}

func refused(t *testing.T, data []byte, l zipcheck.Limits, want error) *zipcheck.Refusal {
	t.Helper()
	err, _ := check(t, data, l)
	var refusal *zipcheck.Refusal
	if !errors.Is(err, want) || !errors.As(err, &refusal) {
		t.Fatalf("err = %v, want a refusal as %v", err, want)
	}
	return refusal
}

// file is one member of an archive a test builds.
type file struct {
	name   string
	data   []byte
	method uint16
	// sized writes the sizes in the local header, with no data
	// descriptor after the data, as Info-ZIP and 7-Zip do. Otherwise the
	// member is written as Go's archive/zip streams it: sizes in a data
	// descriptor.
	sized bool
}

func stored(name string, data []byte) file { return file{name: name, data: data, method: zip.Store} }
func deflated(name string, data []byte) file {
	return file{name: name, data: data, method: zip.Deflate}
}

func build(t *testing.T, files ...file) []byte {
	t.Helper()
	var out bytes.Buffer
	w := zip.NewWriter(&out)
	for _, f := range files {
		if !f.sized {
			m, err := w.CreateHeader(&zip.FileHeader{Name: f.name, Method: f.method})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := m.Write(f.data); err != nil {
				t.Fatal(err)
			}
			continue
		}
		body := f.data
		if f.method == zip.Deflate {
			body = deflate(t, f.data)
		}
		m, err := w.CreateRaw(&zip.FileHeader{
			Name: f.name, Method: f.method, CRC32: crc32.ChecksumIEEE(f.data),
			CompressedSize64: uint64(len(body)), UncompressedSize64: uint64(len(f.data)),
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := m.Write(body); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func deflate(t *testing.T, data []byte) []byte {
	t.Helper()
	var out bytes.Buffer
	w, err := flate.NewWriter(&out, flate.BestCompression)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

// Offsets of the fields a test patches, in a directory entry and in a local
// header.
const (
	dirFlags, dirMethod, dirCRC, dirCSize, dirUSize, dirNameLen, dirExtraLen, dirOffset = 8, 10, 16, 20, 24, 28, 30, 42
	locFlags, locMethod, locCRC, locCSize, locUSize                                     = 6, 8, 14, 18, 22
)

// layout is where an archive a test built keeps its records: the end
// record, and each member's directory entry and local header, found by
// walking the directory from the end record (the tests' data holds no
// signatures).
type layout struct {
	end   int
	dir   []int
	local []int
}

func layoutOf(t *testing.T, data []byte) layout {
	t.Helper()
	end := bytes.LastIndex(data, []byte("PK\x05\x06"))
	if end < 0 {
		t.Fatal("no end record")
	}
	l := layout{end: end}
	p := int(binary.LittleEndian.Uint32(data[end+16:]))
	for range int(binary.LittleEndian.Uint16(data[end+10:])) {
		l.dir = append(l.dir, p)
		l.local = append(l.local, int(binary.LittleEndian.Uint32(data[p+dirOffset:])))
		p += 46 + int(binary.LittleEndian.Uint16(data[p+dirNameLen:])) + int(binary.LittleEndian.Uint16(data[p+dirExtraLen:])) +
			int(binary.LittleEndian.Uint16(data[p+32:]))
	}
	return l
}

func put16(data []byte, at int, v uint16) { binary.LittleEndian.PutUint16(data[at:], v) }
func put32(data []byte, at int, v uint32) { binary.LittleEndian.PutUint32(data[at:], v) }

func zeros(n int) []byte { return make([]byte, n) }

// text is n bytes that deflate barely compresses.
func text(n int) []byte {
	out := make([]byte, n)
	x := uint32(2463534242)
	for i := range out {
		x ^= x << 13
		x ^= x >> 17
		x ^= x << 5
		out[i] = byte(x)
	}
	return out
}

// A ZIP as common tools write it passes: stored and deflated members,
// sizes in the local header or in a data descriptor, a directory, an empty
// member, a name in UTF-8, and the archive's comment. So does an empty ZIP.
func TestCheckPassesAWellFormedZIP(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	w := zip.NewWriter(&out)
	for _, f := range []file{
		deflated("notlar/ders 1.txt", bytes.Repeat([]byte("SKY LAB "), 5000)),
		stored("veri/örnek.bin", text(3000)),
		deflated("veri/boş.txt", nil),
	} {
		m, err := w.CreateHeader(&zip.FileHeader{Name: f.name, Method: f.method})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := m.Write(f.data); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := w.Create("boş klasör/"); err != nil {
		t.Fatal(err)
	}
	for _, f := range []file{
		{name: "sized/deflated.txt", data: bytes.Repeat([]byte("abc"), 4000), method: zip.Deflate, sized: true},
		{name: "sized/stored.bin", data: text(1000), method: zip.Store, sized: true},
	} {
		body := f.data
		if f.method == zip.Deflate {
			body = deflate(t, f.data)
		}
		m, err := w.CreateRaw(&zip.FileHeader{Name: f.name, Method: f.method, CRC32: crc32.ChecksumIEEE(f.data),
			CompressedSize64: uint64(len(body)), UncompressedSize64: uint64(len(f.data))})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := m.Write(body); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.SetComment("SKY LAB etkinlik dosyaları"); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	passes(t, out.Bytes(), limits)
	passes(t, build(t), limits)
}

// A file that is no ZIP, or one cut short, is refused as invalid: its end
// record is missing, or the directory it names is not there.
func TestCheckRefusesAGarbageOrTruncatedFile(t *testing.T) {
	t.Parallel()
	whole := build(t, deflated("a.txt", bytes.Repeat([]byte("a"), 2000)), stored("b.bin", text(500)))
	for name, data := range map[string][]byte{
		"garbage":               append([]byte("PK\x03\x04"), text(4000)...),
		"shorter than a record": []byte("PK\x05\x06"),
		"cut before its end":    whole[:len(whole)-10],
		"cut in its members":    whole[:len(whole)/2],
		"bytes after its end":   append(append([]byte{}, whole...), "trailing"...),
		"empty":                 {},
	} {
		t.Run(name, func(t *testing.T) {
			refused(t, data, limits, zipcheck.ErrInvalid)
		})
	}
}

// A member that inflates past MaxFileSize is refused as too large: clamd
// would skip it without a report. So is one whose header only says so (a
// stored member with a lying header), and the directory tells it before any
// member is read.
func TestCheckRefusesAMemberOverMaxFileSize(t *testing.T) {
	t.Parallel()
	refused(t, build(t, deflated("zeros.bin", zeros(2<<20))), limits, zipcheck.ErrTooLarge)
	refused(t, build(t, file{name: "zeros.bin", data: zeros(2 << 20), method: zip.Deflate, sized: true}), limits, zipcheck.ErrTooLarge)

	lying := build(t, file{name: "big.bin", data: text(1000), method: zip.Store, sized: true})
	l := layoutOf(t, lying)
	for _, at := range []int{l.dir[0] + dirCSize, l.dir[0] + dirUSize, l.local[0] + locCSize, l.local[0] + locUSize} {
		put32(lying, at, 1<<20+1)
	}
	refused(t, lying, limits, zipcheck.ErrTooLarge)

	// Exactly MaxFileSize is scanned whole.
	passes(t, build(t, deflated("zeros.bin", zeros(1<<20))), limits)
}

// Members that inflate past MaxScanSize together, or more members than
// MaxFiles, are refused as too large.
func TestCheckRefusesMoreThanMaxScanSizeOrMaxFiles(t *testing.T) {
	t.Parallel()
	var four []file
	for i := range 5 {
		four = append(four, deflated(fmt.Sprintf("part-%d.bin", i), zeros(900<<10)))
	}
	refused(t, build(t, four...), limits, zipcheck.ErrTooLarge)
	passes(t, build(t, four[:4]...), limits)

	few := zipcheck.Limits{MaxFileSize: 1 << 20, MaxScanSize: 4 << 20, MaxFiles: 3, MaxRecursion: 17}
	many := []file{stored("a", nil), stored("b", nil), stored("c", nil), stored("d", nil)}
	refused(t, build(t, many...), few, zipcheck.ErrTooLarge)
	passes(t, build(t, many[:3]...), few)
}

// A ZIP the directory already refuses is refused from the directory alone:
// the check reads the end of the file in bounded ranges and never streams
// its members.
func TestCheckReadsOnlyTheDirectoryOfAZIPItRefusesByIt(t *testing.T) {
	t.Parallel()
	data := build(t, stored("a.bin", text(3<<20)), stored("b.bin", text(3<<20)))
	err, src := check(t, data, limits)
	if !errors.Is(err, zipcheck.ErrTooLarge) {
		t.Fatalf("err = %v, want %v", err, zipcheck.ErrTooLarge)
	}
	var read int64
	for _, r := range src.reads {
		read += r.n
	}
	if src.largest() > 1<<20 || read > 128<<10 {
		t.Fatalf("read %d bytes of %d in %d ranges (largest %d) to refuse it", read, len(data), len(src.reads), src.largest())
	}
}

// withoutEntry is the archive with the directory entry of member k taken
// out: its local header and data stay, listed by nothing.
func withoutEntry(t *testing.T, data []byte, k int) []byte {
	t.Helper()
	l := layoutOf(t, data)
	endOfEntry := l.end
	if k+1 < len(l.dir) {
		endOfEntry = l.dir[k+1]
	}
	out := append(append([]byte{}, data[:l.dir[k]]...), data[endOfEntry:]...)
	end := l.end - (endOfEntry - l.dir[k])
	put16(out, end+8, uint16(len(l.dir)-1))
	put16(out, end+10, uint16(len(l.dir)-1))
	put32(out, end+12, binary.LittleEndian.Uint32(out[end+12:])-uint32(endOfEntry-l.dir[k]))
	return out
}

// duplicated is the archive with member 0's directory entry listed n times:
// every copy points at the same local header, the classic overlap bomb.
func duplicated(t *testing.T, data []byte, n int) []byte {
	t.Helper()
	l := layoutOf(t, data)
	if len(l.dir) != 1 {
		t.Fatal("duplicated wants a single member")
	}
	entry := data[l.dir[0]:l.end]
	out := append([]byte{}, data[:l.end]...)
	for range n - 1 {
		out = append(out, entry...)
	}
	end := len(out)
	out = append(out, data[l.end:]...)
	put16(out, end+8, uint16(n))
	put16(out, end+10, uint16(n))
	put32(out, end+12, uint32(n*len(entry)))
	return out
}

// Members that overlap are refused as invalid from the directory alone: the
// same local header listed many times (the classic overlap bomb), or a
// member whose data runs over the next member's (the quoted overlap).
func TestCheckRefusesOverlappingMembers(t *testing.T) {
	t.Parallel()
	bomb := duplicated(t, build(t, file{name: "a.txt", data: text(100 << 10), method: zip.Store, sized: true}), 30)
	err, src := check(t, bomb, limits)
	if !errors.Is(err, zipcheck.ErrInvalid) {
		t.Fatalf("overlap bomb: err = %v, want %v", err, zipcheck.ErrInvalid)
	}
	for _, r := range src.reads {
		if r.off == 0 {
			t.Fatalf("the overlap bomb's members were streamed (%+v)", src.reads)
		}
	}

	quoted := build(t, file{name: "a.txt", data: text(2000), method: zip.Store, sized: true},
		file{name: "b.txt", data: zeros(50 << 10), method: zip.Deflate, sized: true})
	l := layoutOf(t, quoted)
	covering := uint32(l.dir[0] - l.local[0] - 30 - len("a.txt"))
	put32(quoted, l.dir[0]+dirCSize, covering)
	put32(quoted, l.dir[0]+dirUSize, covering)
	refused(t, quoted, limits, zipcheck.ErrInvalid)
}

// A directory entry that points outside the members (past the file, or into
// the directory), bytes before the first member, and a member's bytes the
// directory does not list are refused as invalid: a reader walking local
// headers would find what clamd, reading the directory, does not.
func TestCheckRefusesBytesNoMemberAccountsFor(t *testing.T) {
	t.Parallel()
	two := func() []byte {
		return build(t, file{name: "a.txt", data: text(1000), method: zip.Store, sized: true},
			file{name: "b.txt", data: text(1000), method: zip.Deflate, sized: true})
	}
	past := two()
	put32(past, layoutOf(t, past).dir[1]+dirOffset, 0x7ffffff0)
	intoDirectory := two()
	l := layoutOf(t, intoDirectory)
	put32(intoDirectory, l.dir[1]+dirOffset, uint32(l.dir[0]))

	plain := two()
	l = layoutOf(t, plain)
	prefix := "MZ-self-extractor"
	prefixed := append([]byte(prefix), plain...)
	for _, at := range append(l.dir, l.end+16-dirOffset) {
		at += len(prefix) + dirOffset
		put32(prefixed, at, binary.LittleEndian.Uint32(prefixed[at:])+uint32(len(prefix)))
	}

	hidden := withoutEntry(t, build(t,
		file{name: "a.txt", data: text(1000), method: zip.Store, sized: true},
		file{name: "hidden.txt", data: text(1000), method: zip.Deflate, sized: true},
		file{name: "c.txt", data: text(1000), method: zip.Store, sized: true}), 1)

	emptyAfterBytes := append([]byte("junk"), build(t)...)
	put32(emptyAfterBytes, 4+16, 4)

	for name, data := range map[string][]byte{
		"an entry past the file":          past,
		"an entry in the directory":       intoDirectory,
		"bytes before the first member":   prefixed,
		"a member the directory omits":    hidden,
		"bytes before an empty directory": emptyAfterBytes,
	} {
		t.Run(name, func(t *testing.T) {
			refused(t, data, limits, zipcheck.ErrInvalid)
		})
	}
}

// withZip64End is the archive with a ZIP64 end record and its locator
// between the directory and the end record, as writers that always write
// them do; change may alter the record or the locator.
func withZip64End(t *testing.T, data []byte, change func(record, locator []byte)) []byte {
	t.Helper()
	l := layoutOf(t, data)
	le := binary.LittleEndian
	record := make([]byte, 56)
	le.PutUint32(record, 0x06064b50)
	le.PutUint64(record[4:], 44)
	le.PutUint16(record[12:], 45)
	le.PutUint16(record[14:], 45)
	le.PutUint64(record[24:], uint64(len(l.dir)))
	le.PutUint64(record[32:], uint64(len(l.dir)))
	le.PutUint64(record[40:], uint64(le.Uint32(data[l.end+12:])))
	le.PutUint64(record[48:], uint64(le.Uint32(data[l.end+16:])))
	locator := make([]byte, 20)
	le.PutUint32(locator, 0x07064b50)
	le.PutUint64(locator[8:], uint64(l.end))
	le.PutUint32(locator[16:], 1)
	if change != nil {
		change(record, locator)
	}
	out := append([]byte{}, data[:l.end]...)
	out = append(append(out, record...), locator...)
	return append(out, data[l.end:]...)
}

// zip64Member is a single stored member written with ZIP64 sizes: 0xFFFFFFFF
// in the headers, the real sizes (here size, which may lie) in the ZIP64
// extra field.
func zip64Member(data []byte, size uint64) []byte {
	le := binary.LittleEndian
	name := "e.txt"
	extra := make([]byte, 20)
	le.PutUint16(extra, 1)
	le.PutUint16(extra[2:], 16)
	le.PutUint64(extra[4:], size)
	le.PutUint64(extra[12:], size)
	var out bytes.Buffer
	local := make([]byte, 30)
	le.PutUint32(local, 0x04034b50)
	le.PutUint16(local[4:], 45)
	le.PutUint32(local[14:], crc32.ChecksumIEEE(data))
	le.PutUint32(local[18:], 0xffffffff)
	le.PutUint32(local[22:], 0xffffffff)
	le.PutUint16(local[26:], uint16(len(name)))
	le.PutUint16(local[28:], uint16(len(extra)))
	out.Write(local)
	out.WriteString(name)
	out.Write(extra)
	out.Write(data)
	dirAt := out.Len()
	entry := make([]byte, 46)
	le.PutUint32(entry, 0x02014b50)
	le.PutUint16(entry[4:], 45)
	le.PutUint16(entry[6:], 45)
	le.PutUint32(entry[16:], crc32.ChecksumIEEE(data))
	le.PutUint32(entry[20:], 0xffffffff)
	le.PutUint32(entry[24:], 0xffffffff)
	le.PutUint16(entry[28:], uint16(len(name)))
	le.PutUint16(entry[30:], uint16(len(extra)))
	out.Write(entry)
	out.WriteString(name)
	out.Write(extra)
	end := make([]byte, 22)
	le.PutUint32(end, 0x06054b50)
	le.PutUint16(end[8:], 1)
	le.PutUint16(end[10:], 1)
	le.PutUint32(end[12:], uint32(out.Len()-dirAt))
	le.PutUint32(end[16:], uint32(dirAt))
	out.Write(end)
	return out.Bytes()
}

// ZIP64 is only needed past 4 GiB, which no file core scans reaches, and
// clamd 1.5.4 does not read ZIP64 sizes: it passed a member written with
// them unscanned. A member with ZIP64 sizes, true or lying, an end record
// that leaves its counts to ZIP64, and a ZIP64 end record that disagrees
// with the end record or is not where its locator says are refused as
// invalid. A ZIP64 end record that repeats the end record passes.
func TestCheckRefusesZIP64Lies(t *testing.T) {
	t.Parallel()
	plain := func() []byte {
		return build(t, deflated("a.txt", bytes.Repeat([]byte("a"), 3000)), stored("b.bin", text(700)))
	}
	passes(t, withZip64End(t, plain(), nil), limits)

	sentinelCount := plain()
	l := layoutOf(t, sentinelCount)
	put16(sentinelCount, l.end+10, 0xffff)
	put16(sentinelCount, l.end+8, 0xffff)

	for name, data := range map[string][]byte{
		"a member with ZIP64 sizes":         zip64Member(text(68), 68),
		"a member whose ZIP64 size lies":    zip64Member(text(68), 7),
		"counts left to ZIP64":              sentinelCount,
		"a ZIP64 end record with 3 members": withZip64End(t, plain(), func(record, _ []byte) { binary.LittleEndian.PutUint64(record[32:], 3) }),
		"a ZIP64 end record with another directory size": withZip64End(t, plain(), func(record, _ []byte) {
			binary.LittleEndian.PutUint64(record[40:], binary.LittleEndian.Uint64(record[40:])+1)
		}),
		"a ZIP64 end record elsewhere": withZip64End(t, plain(), func(_, locator []byte) {
			binary.LittleEndian.PutUint64(locator[8:], binary.LittleEndian.Uint64(locator[8:])-1)
		}),
		"a ZIP64 end record on another disk": withZip64End(t, plain(), func(_, locator []byte) { binary.LittleEndian.PutUint32(locator[16:], 2) }),
		"a ZIP64 end record that is not one": withZip64End(t, plain(), func(record, _ []byte) { record[0] = 'X' }),
	} {
		t.Run(name, func(t *testing.T) {
			refused(t, data, limits, zipcheck.ErrInvalid)
		})
	}
}

// An encrypted member (flag bit 0, or strong encryption, or a masked
// directory), whatever header says it, is refused as invalid: clamd cannot
// read it, and AlertEncrypted is off. So is a member compressed with a
// method other than stored or deflate (clamd 1.5.4 passed one with an
// unknown method unscanned), and one whose name is empty, longer than 1024
// bytes, or holds a NUL.
func TestCheckRefusesMembersClamdCannotRead(t *testing.T) {
	t.Parallel()
	patched := func(change func(data []byte, l layout)) []byte {
		data := build(t, file{name: "a.txt", data: text(1000), method: zip.Store, sized: true},
			file{name: "b.txt", data: bytes.Repeat([]byte("b"), 1000), method: zip.Deflate, sized: true})
		change(data, layoutOf(t, data))
		return data
	}
	cases := map[string][]byte{}
	for _, flag := range []uint16{1 << 0, 1 << 6, 1 << 13} {
		cases[fmt.Sprintf("flag %#x in both headers", flag)] = patched(func(data []byte, l layout) {
			put16(data, l.dir[1]+dirFlags, flag)
			put16(data, l.local[1]+locFlags, flag)
		})
		cases[fmt.Sprintf("flag %#x in the local header only", flag)] = patched(func(data []byte, l layout) {
			put16(data, l.local[1]+locFlags, flag)
		})
	}
	for _, method := range []uint16{1, 6, 9, 12, 14, 93, 99} {
		cases[fmt.Sprintf("method %d", method)] = patched(func(data []byte, l layout) {
			put16(data, l.dir[1]+dirMethod, method)
			put16(data, l.local[1]+locMethod, method)
		})
	}
	cases["an empty name"] = build(t, stored("", []byte("x")))
	cases["a name of 1025 bytes"] = build(t, stored(string(bytes.Repeat([]byte("n"), 1025)), []byte("x")))
	cases["a name with a NUL"] = build(t, stored("a.txt\x00.pdf", []byte("x")))
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			refused(t, data, limits, zipcheck.ErrInvalid)
		})
	}
	passes(t, build(t, stored(string(bytes.Repeat([]byte("n"), 1024)), []byte("x"))), limits)
}

// clamd goes by what a member really inflates to: a member whose headers
// both say 100 bytes but that inflates past MaxFileSize was skipped
// unscanned by clamd 1.5.4. Every member is inflated, never kept, and one
// that inflates to more or less than it declares, whose checksum differs,
// whose local header or data descriptor disagrees with the directory, or
// that carries bytes after its deflate stream is refused as invalid.
func TestCheckRefusesMembersThatLieAboutWhatTheyHold(t *testing.T) {
	t.Parallel()
	sized := func(change func(data []byte, l layout)) []byte {
		data := build(t, file{name: "zeros.bin", data: zeros(2 << 20), method: zip.Deflate, sized: true},
			file{name: "b.txt", data: text(500), method: zip.Store, sized: true})
		change(data, layoutOf(t, data))
		return data
	}
	streamed := func(change func(data []byte, l layout)) []byte {
		data := build(t, deflated("small.txt", bytes.Repeat([]byte("s"), 5000)), stored("b.txt", text(500)))
		change(data, layoutOf(t, data))
		return data
	}
	cases := map[string][]byte{
		"both headers say 100 bytes of 2 MiB": sized(func(data []byte, l layout) {
			put32(data, l.dir[0]+dirUSize, 100)
			put32(data, l.local[0]+locUSize, 100)
		}),
		"the local header says 100 bytes": sized(func(data []byte, l layout) { put32(data, l.local[0]+locUSize, 100) }),
		"the local header says another checksum": sized(func(data []byte, l layout) {
			put32(data, l.local[0]+locCRC, 1)
		}),
		"both headers say another checksum": sized(func(data []byte, l layout) {
			put32(data, l.dir[0]+dirCRC, 1)
			put32(data, l.local[0]+locCRC, 1)
		}),
		"the directory says 2 MiB and a byte": sized(func(data []byte, l layout) {
			put32(data, l.dir[0]+dirUSize, 2<<20+1)
			put32(data, l.local[0]+locUSize, 2<<20+1)
		}),
		"the data descriptor says another size": streamed(func(data []byte, l layout) {
			at := bytes.Index(data[l.local[0]:], []byte("PK\x07\x08"))
			put32(data, l.local[0]+at+12, 4999)
		}),
		"the local header gives sizes the directory does not": streamed(func(data []byte, l layout) {
			put32(data, l.local[0]+locUSize, 4999)
		}),
	}
	big := zipcheck.Limits{MaxFileSize: 4 << 20, MaxScanSize: 8 << 20, MaxFiles: 100, MaxRecursion: 17}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			refused(t, data, big, zipcheck.ErrInvalid)
		})
	}

	// A deflate stream with bytes after its end inside the member's data.
	body := append(deflate(t, []byte("hello")), "hidden"...)
	var out bytes.Buffer
	w := zip.NewWriter(&out)
	m, err := w.CreateRaw(&zip.FileHeader{Name: "a.txt", Method: zip.Deflate, CRC32: crc32.ChecksumIEEE([]byte("hello")),
		CompressedSize64: uint64(len(body)), UncompressedSize64: 5})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	refused(t, out.Bytes(), big, zipcheck.ErrInvalid)
	passes(t, sized(func([]byte, layout) {}), big)
}

// An archive inside the ZIP is accepted one level deep, and only when it is
// a ZIP core reads the same way: clamd scans what is inside it too, and
// skips there what it skips at the top (checked: a nested ZIP's member past
// MaxFileSize went unscanned). Its members count toward the same limits.
// An archive inside that one, an archive of another format (by its name or
// its first bytes), and any nesting while clamd's MaxRecursion would not
// reach the members of a nested ZIP are refused as too large.
func TestCheckNestedArchives(t *testing.T) {
	t.Parallel()
	inner := build(t, deflated("a.txt", bytes.Repeat([]byte("a"), 4000)), stored("b.bin", text(300)))
	gzipped := append([]byte{0x1f, 0x8b, 8, 0}, text(200)...)
	tarball := make([]byte, 1024)
	copy(tarball, "data.csv")
	copy(tarball[257:], "ustar\x0000")

	passes(t, build(t, deflated("inner.zip", inner), stored("notes.txt", []byte("notes"))), limits)
	passes(t, build(t, stored("inner.zip", inner)), limits)
	passes(t, build(t, deflated("rapor.docx", inner), deflated("slides.PPTX", inner)), limits)
	passes(t, build(t, stored("empty.zip", nil), deflated("data.csv", []byte("a,b\n1,2\n"))), limits)

	lyingInner := build(t, file{name: "zeros.bin", data: zeros(300 << 10), method: zip.Deflate, sized: true})
	l := layoutOf(t, lyingInner)
	put32(lyingInner, l.dir[0]+dirUSize, 100)
	put32(lyingInner, l.local[0]+locUSize, 100)
	cutInner := append(append([]byte{}, inner...), "tail"...)

	for name, c := range map[string]struct {
		data   []byte
		limits zipcheck.Limits
		want   error
	}{
		"a ZIP in a ZIP in the ZIP": {build(t, deflated("inner.zip", build(t, deflated("deeper.zip", inner)))), limits, zipcheck.ErrTooLarge},
		"a ZIP named as another in a ZIP in the ZIP": {
			build(t, deflated("inner.zip", build(t, deflated("deeper.bin", inner)))), limits, zipcheck.ErrTooLarge,
		},
		"a nested member past MaxFileSize": {build(t, deflated("inner.zip", build(t, deflated("zeros.bin", zeros(2<<20))))), limits, zipcheck.ErrTooLarge},
		"a nested member that lies":        {build(t, deflated("inner.zip", lyingInner)), limits, zipcheck.ErrInvalid},
		"a nested ZIP with bytes after its end": {
			build(t, deflated("inner.zip", cutInner)), limits, zipcheck.ErrInvalid,
		},
		"more members than MaxFiles in all": {
			build(t, deflated("inner.zip", inner), stored("c.txt", nil)),
			zipcheck.Limits{MaxFileSize: 1 << 20, MaxScanSize: 4 << 20, MaxFiles: 3, MaxRecursion: 17}, zipcheck.ErrTooLarge,
		},
		"a gzip by its first bytes": {build(t, deflated("data.bin", gzipped)), limits, zipcheck.ErrTooLarge},
		"a tar by its first bytes":  {build(t, deflated("data", tarball)), limits, zipcheck.ErrTooLarge},
		"a 7z by its name":          {build(t, stored("backup.7z", text(100))), limits, zipcheck.ErrTooLarge},
		"a RAR by its name":         {build(t, stored("backup.RAR", text(100))), limits, zipcheck.ErrTooLarge},
		"a ZIP by its name only":    {build(t, stored("not-really.zip", text(100))), limits, zipcheck.ErrTooLarge},
		"a tar.gz by its name":      {build(t, stored("veri.tar.gz", text(100))), limits, zipcheck.ErrTooLarge},
		"a ZIP where MaxRecursion is 2": {
			build(t, deflated("inner.zip", inner)),
			zipcheck.Limits{MaxFileSize: 1 << 20, MaxScanSize: 4 << 20, MaxFiles: 100, MaxRecursion: 2}, zipcheck.ErrTooLarge,
		},
	} {
		t.Run(name, func(t *testing.T) {
			refused(t, c.data, c.limits, c.want)
		})
	}
}

// A nested ZIP larger than what the check keeps of a member's end (8 MiB)
// is read from its last bytes all the same.
func TestCheckReadsTheDirectoryOfALargeNestedZIPFromItsEnd(t *testing.T) {
	t.Parallel()
	inner := build(t, stored("a.bin", text(5<<20)), deflated("b.txt", bytes.Repeat([]byte("b"), 4<<20)), stored("c.bin", text(1<<20)))
	roomy := zipcheck.Limits{MaxFileSize: 16 << 20, MaxScanSize: 64 << 20, MaxFiles: 100, MaxRecursion: 17}
	passes(t, build(t, deflated("inner.zip", inner)), roomy)
	passes(t, build(t, stored("inner.zip", inner)), roomy)
}

// A refusal names members by their place, never by their names, so it can
// be logged.
func TestCheckRefusalNamesNoMember(t *testing.T) {
	t.Parallel()
	r := refused(t, build(t, stored("a.txt", nil), deflated("Ada Lovelace notlar.bin", zeros(2<<20))), limits, zipcheck.ErrTooLarge)
	if r.Reason != "member 2 inflates to 2097152 bytes, more than MaxFileSize (1048576)" {
		t.Fatalf("reason %q", r.Reason)
	}
	nested := build(t, deflated("iç.zip", build(t, stored("a", nil), deflated("Ada.bin", zeros(2<<20)))))
	if r := refused(t, nested, limits, zipcheck.ErrTooLarge); r.Reason != "member 2 of member 1 inflates to 2097152 bytes, more than MaxFileSize (1048576)" {
		t.Fatalf("reason %q", r.Reason)
	}
}

// Limits the check cannot hold a ZIP within, and storage that fails or
// serves less than the size, are errors that say nothing about the ZIP:
// never a refusal.
func TestCheckFailuresAreNoRefusal(t *testing.T) {
	t.Parallel()
	data := build(t, deflated("a.txt", bytes.Repeat([]byte("a"), 3000)))
	for name, l := range map[string]zipcheck.Limits{
		"no MaxFileSize":     {MaxScanSize: 1, MaxFiles: 1, MaxRecursion: 17},
		"no MaxScanSize":     {MaxFileSize: 1, MaxFiles: 1, MaxRecursion: 17},
		"no MaxFiles":        {MaxFileSize: 1, MaxScanSize: 1, MaxRecursion: 17},
		"MaxFiles too large": {MaxFileSize: 1, MaxScanSize: 1, MaxFiles: 1 << 16, MaxRecursion: 17},
		"MaxRecursion 1":     {MaxFileSize: 1, MaxScanSize: 1, MaxFiles: 1, MaxRecursion: 1},
	} {
		if err, _ := check(t, data, l); err == nil || errors.Is(err, zipcheck.ErrTooLarge) || errors.Is(err, zipcheck.ErrInvalid) {
			t.Errorf("%s: err = %v, want an error that is no refusal", name, err)
		}
	}
	outage := errors.New("r2: 503 Service Unavailable")
	if err := zipcheck.Check(context.Background(), &source{data: data, fail: outage}, int64(len(data)), limits); !errors.Is(err, outage) {
		t.Errorf("storage down: err = %v", err)
	}
	// The object holds less than its size: its first bytes are gone.
	short := &shortSource{source{data: data}, 40}
	if err := zipcheck.Check(context.Background(), short, int64(len(data)), limits); err == nil ||
		errors.Is(err, zipcheck.ErrTooLarge) || errors.Is(err, zipcheck.ErrInvalid) {
		t.Errorf("short object: err = %v", err)
	}
}

// shortSource serves a stream from the start that ends early, as a storage
// read cut short does.
type shortSource struct {
	source
	cut int64
}

func (s *shortSource) OpenRange(ctx context.Context, off, n int64) (io.ReadCloser, error) {
	body, err := s.source.OpenRange(ctx, off, n)
	if err != nil || off != 0 {
		return body, err
	}
	return io.NopCloser(io.LimitReader(body, s.cut)), nil
}

// A member is an archive by its name or its first bytes, and only formats
// clamd opens count. What merely starts like one is no archive: a static
// library (ar, which clamd does not open), a text that starts with "BZh" or
// looks like an LHA header, bytes 1f 8b that are no deflate gzip, a cpio-like
// number, and Debian or RPM packages by name.
func TestCheckTakesNoLookalikeForAnArchive(t *testing.T) {
	t.Parallel()
	passes(t, build(t,
		stored("lib/libsky.a", append([]byte("!<arch>\n"), text(200)...)),
		deflated("notlar/bzh.txt", []byte("BZhello, SKY LAB")),
		deflated("scripts/build.sh", []byte("# -lib-x build\n")),
		stored("veri/binary.dat", append([]byte{0x1f, 0x8b, 0x00}, text(100)...)),
		deflated("veri/sayı.txt", []byte("070709 not a cpio archive")),
		stored("paket/sky.deb", text(300)),
		stored("paket/sky.rpm", text(300)),
		stored("rpm-like.bin", append([]byte{0xed, 0xab, 0xee, 0xdb}, text(100)...)),
	), limits)
}

// Real archives of the formats clamd opens are still caught by their first
// bytes, whatever their names.
func TestCheckCatchesRealNestedArchivesByTheirFirstBytes(t *testing.T) {
	t.Parallel()
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	if _, err := zw.Write([]byte("hidden")); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	lha := append([]byte{0x20, 0x00}, "-lh5-"...)
	for name, data := range map[string][]byte{
		"gzip":        gz.Bytes(),
		"bzip2":       append([]byte("BZh91AY&SY"), text(100)...),
		"empty bzip2": []byte("BZh9\x17\x72\x45\x38\x50\x90\x00\x00\x00\x00"),
		"cpio":        append([]byte("070701"), text(100)...),
		"LHA":         append(lha, text(100)...),
		"7-Zip":       append([]byte("7z\xbc\xaf\x27\x1c"), text(100)...),
		"RAR":         append([]byte("Rar!\x1a\x07\x01\x00"), text(100)...),
		"xz":          append([]byte("\xfd7zXZ\x00"), text(100)...),
		"cab":         append([]byte("MSCF\x00\x00\x00\x00"), text(100)...),
	} {
		t.Run(name, func(t *testing.T) {
			refused(t, build(t, deflated("data.bin", data)), limits, zipcheck.ErrTooLarge)
		})
	}
}
