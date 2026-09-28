package zipcheck_test

import (
	"archive/zip"
	"bytes"
	"compress/flate"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"testing"

	"github.com/skylab-kulubu/core-backend/internal/zipcheck"
)

// lone is a local header and its data that no directory lists: a member
// clamd unpacks where it finds it. version, flags, method and name are as
// given; the data is deflated for method 8, stored otherwise.
func lone(version byte, flags, method uint16, name string, content []byte) []byte {
	body := content
	if method == 8 {
		var c bytes.Buffer
		fw, _ := flate.NewWriter(&c, flate.BestCompression)
		fw.Write(content)
		fw.Close()
		body = c.Bytes()
	}
	h := make([]byte, 30)
	copy(h, "PK\x03\x04")
	h[4] = version
	binary.LittleEndian.PutUint16(h[6:], flags)
	binary.LittleEndian.PutUint16(h[8:], method)
	binary.LittleEndian.PutUint32(h[14:], crc32.ChecksumIEEE(content))
	binary.LittleEndian.PutUint32(h[18:], uint32(len(body)))
	binary.LittleEndian.PutUint32(h[22:], uint32(len(content)))
	binary.LittleEndian.PutUint16(h[26:], uint16(len(name)))
	return append(append(h, name...), body...)
}

// hidden is a lone member, as a clamd test probe hid one: a small one, or
// one past MaxFileSize.
func hidden(big bool) []byte {
	if big {
		return lone(20, 0, 8, "h.txt", zeros(2<<20))
	}
	return lone(20, 0, 8, "h.txt", []byte("hidden"))
}

// zipCommented is a small ZIP whose end record's comment is comment.
func zipCommented(t *testing.T, comment []byte) []byte {
	t.Helper()
	var out bytes.Buffer
	w := zip.NewWriter(&out)
	m, err := w.Create("a.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	if err := w.SetComment(string(comment)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

// zipWithExtra is a small ZIP whose member's local header carries extra.
func zipWithExtra(t *testing.T, extra []byte, dirComment string) []byte {
	t.Helper()
	var out bytes.Buffer
	w := zip.NewWriter(&out)
	m, err := w.CreateRaw(&zip.FileHeader{Name: "a.txt", Method: zip.Store, CRC32: crc32.ChecksumIEEE([]byte("hello")),
		CompressedSize64: 5, UncompressedSize64: 5, Extra: extra, Comment: dirComment})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func extraField(data []byte) []byte {
	out := binary.LittleEndian.AppendUint16(nil, 0xcafe)
	out = binary.LittleEndian.AppendUint16(out, uint16(len(data)))
	return append(out, data...)
}

// clamd 1.5.4 unpacks a lone local header wherever it finds one: in a ZIP's
// end record comment or a member's extra field, in a ZIP and in a ZIP
// appended to a PDF (checked), and a large one it scans only up to
// MaxFileSize. No directory lists such a member, so the walk would never
// read it. A local header signature outside the members' data (a local
// header's name or extra field, a data descriptor, the directory, the end
// record's comment) is refused as invalid; the ZIP's own local headers are
// the only ones.
func TestCheckRefusesHiddenLocalHeaders(t *testing.T) {
	t.Parallel()
	for name, data := range map[string][]byte{
		"a small one in the comment":            zipCommented(t, hidden(false)),
		"a large one in the comment":            zipCommented(t, hidden(true)),
		"one in a member's extra field":         zipWithExtra(t, extraField(hidden(false)), ""),
		"a large one in a member's extra field": zipWithExtra(t, extraField(hidden(true)), ""),
		"one in a directory entry's comment":    zipWithExtra(t, nil, string(hidden(false))),
		"a bare signature in a member's name":   build(t, stored("a\x50\x4b\x03\x04.txt", []byte("hello"))),
		"a PDF with a ZIP, one in its comment":  append(pdf(), zipCommented(t, hidden(false))...),
		"a PDF with a ZIP, one in an extra":     append(pdf(), zipWithExtra(t, extraField(hidden(true)), "")...),
	} {
		t.Run(name, func(t *testing.T) {
			if err := checkFile(t, data, limits); !errors.Is(err, zipcheck.ErrInvalid) {
				t.Fatalf("err = %v, want %v", err, zipcheck.ErrInvalid)
			}
		})
	}
}

// Past a file's first byte, a local header counts as clamd unpacks one: it
// goes by the signature, a method the format defines, and a name, extra
// field and data that fit in the rest of the file, whatever the header's
// version, flags or name length say (checked: clamd unpacked each below).
// A lone one, which is no ZIP ending the file, is refused as nested.
func TestCheckTakesAnyLocalHeaderClamdUnpacks(t *testing.T) {
	t.Parallel()
	small := []byte("hidden")
	for name, member := range map[string][]byte{
		"version 2.0":          lone(20, 0, 8, "h.txt", small),
		"version 25.5":         lone(255, 0, 8, "h.txt", small),
		"no name":              lone(0, 0, 8, "", small),
		"a name of 2000 bytes": lone(20, 0, 8, string(bytes.Repeat([]byte("n"), 2000)), small),
		"a reserved flag":      lone(20, 1<<15, 8, "h.txt", small),
		"stored":               lone(20, 0, 0, "h.txt", small),
		"past MaxFileSize":     lone(20, 0, 8, "h.txt", zeros(2<<20)),
	} {
		t.Run(name, func(t *testing.T) {
			for where, data := range map[string][]byte{
				"appended to a PDF":   append(pdf(), member...),
				"in the middle of it": append(append(pdf(), member...), pdf()...),
				"in a member":         build(t, deflated("setup.exe", append(stub(1000), member...))),
			} {
				if err := checkFile(t, data, limits); err == nil {
					t.Errorf("%s: passed", where)
				}
			}
		})
	}
	// A local header signature whose method the format does not define, or
	// whose data would run past the file, is none clamd unpacks.
	undefined := lone(20, 0, 77, "h.txt", small)
	runsPast := lone(20, 0, 0, "h.txt", small)
	binary.LittleEndian.PutUint32(runsPast[18:], 1<<30)
	for name, data := range map[string][]byte{
		"an undefined method": append(pdf(), undefined...),
		"data past the file":  append(pdf(), runsPast...),
	} {
		t.Run(name, func(t *testing.T) {
			if err := checkFile(t, data, limits); err != nil {
				t.Fatalf("refused: %v", err)
			}
		})
	}
}
