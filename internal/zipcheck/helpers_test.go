package zipcheck_test

import (
	"archive/zip"
	"bytes"
	"compress/flate"
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
