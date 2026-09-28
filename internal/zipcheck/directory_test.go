package zipcheck_test

import (
	"archive/zip"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"testing"

	"github.com/skylab-kulubu/core-backend/internal/zipcheck"
)

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
