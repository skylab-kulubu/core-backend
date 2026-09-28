package zipcheck_test

import (
	"archive/zip"
	"bytes"
	"fmt"
	"hash/crc32"
	"testing"

	"github.com/skylab-kulubu/core-backend/internal/zipcheck"
)

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
