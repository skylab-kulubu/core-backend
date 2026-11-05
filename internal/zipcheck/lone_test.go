package zipcheck_test

import (
	"bytes"
	"compress/flate"
	"encoding/binary"
	"errors"
	"testing"

	"github.com/skylab-kulubu/core-backend/internal/zipcheck"
)

// pdfStream is a PDF whose first object is a stream starting at byte at,
// holding body: an embedded file stored as it is, or any other binary
// stream. More of the PDF follows, and it ends as a PDF does.
func pdfStream(at int, body []byte) []byte {
	head := []byte("%PDF-1.7\n%\xe2\xe3\xcf\xd3\n1 0 obj\n<< /Type /EmbeddedFile /Length 0 ")
	tail := []byte(">>\nstream\n")
	pad := at - len(head) - len(tail)
	if pad < 0 {
		panic("pdfStream: at is too small")
	}
	out := append(append(append([]byte{}, head...), bytes.Repeat([]byte(" "), pad)...), tail...)
	out = append(append(out, body...), "\nendstream\nendobj\n"...)
	return append(out, pdf()[9:]...)
}

// localHeader is a local header signature followed by what chance data or
// another format put there: version 2.0, flags, method, the sizes given,
// no name, and data.
func localHeader(flags, method uint16, csize, usize uint32, data []byte) []byte {
	h := make([]byte, 30)
	copy(h, "PK\x03\x04")
	h[4] = 20
	binary.LittleEndian.PutUint16(h[6:], flags)
	binary.LittleEndian.PutUint16(h[8:], method)
	binary.LittleEndian.PutUint32(h[18:], csize)
	binary.LittleEndian.PutUint32(h[22:], usize)
	return append(h, data...)
}

// valid is a lone member clamd unpacks whole: its sizes in its header.
func valid(method uint16, content []byte) []byte {
	return lone(20, 0, method, "a.txt", content)
}

// brokenAfter is a deflate stream that yields content, then breaks off.
func brokenAfter(t *testing.T, content []byte) []byte {
	t.Helper()
	var b bytes.Buffer
	fw, err := flate.NewWriter(&b, flate.BestCompression)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := fw.Flush(); err != nil {
		t.Fatal(err)
	}
	return append(b.Bytes(), 0xff, 0xff, 0xff, 0xff)
}

// padding is n spaces: no signature, no archive.
func padding(n int) []byte { return bytes.Repeat([]byte(" "), n) }

// Production refused a PDF with a local header signature at byte 141 as
// archive_nested, every try. Past a file's first byte, a local header
// signature that no end record follows passes when it is chance bytes
// (nothing clamd or a reader unpacks from it) or a member clamd unpacks
// whole, within the limits. A ZIP in the middle of a PDF, its sizes in its
// local headers, passes: checked whole when its end record is among the
// file's last 65,557 bytes, member by member when it is not.
func TestCheckFilePassesChanceLocalHeadersAndWholeMembers(t *testing.T) {
	t.Parallel()
	odf := build(t, sized(stored("mimetype", []byte("application/vnd.oasis.opendocument.text")),
		deflated("content.xml", text(20<<10)), deflated("styles.xml", bytes.Repeat([]byte("<style/>"), 1000)))...)
	for name, data := range map[string][]byte{
		"141: random bytes":             pdfStream(141, append([]byte("PK\x03\x04"), text(4000)...)),
		"141: an undefined method":      pdfStream(141, localHeader(0, 77, 64, 1000, text(200))),
		"141: sizes past the file":      pdfStream(141, localHeader(0, 8, 1<<30, 1<<30, text(200))),
		"141: a stream that breaks":     pdfStream(141, localHeader(0, 8, 64, 1000, append([]byte{0xff}, text(200)...))),
		"141: a stored member":          pdfStream(141, valid(0, []byte("ek notlar"))),
		"141: a deflated member":        pdfStream(141, valid(8, text(4000))),
		"a ZIP mid-PDF":                 pdfStream(141, odf),
		"a ZIP mid-PDF, 70 KiB after":   pdfStream(141, append(append([]byte{}, odf...), padding(70<<10)...)),
		"a ZIP, then a member":          pdfStream(141, append(append(append([]byte{}, odf...), padding(100)...), valid(8, text(1000))...)),
		"a ZIP mid-PDF, in a member":    build(t, deflated("rapor.pdf", pdfStream(141, odf))),
		"a header past the end, gzip":   build(t, stored("a.gz", gzOf(t, append(pdf(), localHeader(0, 0, 1<<20, 1<<20, text(10))...)))),
		"members mid-PDF, 70 KiB after": pdfStream(141, append(append(valid(0, []byte("a")), valid(8, text(1000))...), padding(70<<10)...)),
	} {
		t.Run(name, func(t *testing.T) {
			if err := checkFile(t, data, limits); err != nil {
				t.Fatalf("refused: %v", err)
			}
		})
	}
}

// A local header past a file's first byte that clamd would not unpack
// whole, or a ZIP there it would not read whole, is refused; one past the
// limits as too large.
func TestCheckFileRefusesWhatClamdWouldNotUnpackWhole(t *testing.T) {
	t.Parallel()
	bomb := deflate(t, zeros(2<<20))
	payload := deflate(t, text(4000))
	small := build(t, sized(deflated("notlar.txt", []byte("ek notlar")))...)
	big := build(t, sized(deflated("zeros.bin", zeros(2<<20)))...)
	streamed := build(t, deflated("content.xml", text(2000)))
	rar := build(t, sized(stored("a.rar", append([]byte("Rar!\x1a\x07\x00"), text(100)...)))...)
	gz := build(t, sized(deflated("a.gz", gzOf(t, zeros(2<<20))))...)
	overrun := append([]byte{}, small...)
	put16(overrun, len(overrun)-2, 1000)
	tight := limits
	tight.MaxFiles = 2
	for name, c := range map[string]struct {
		data   []byte
		limits zipcheck.Limits
		want   error
	}{
		"descriptor, deflated":                      {pdfStream(141, localHeader(8, 8, 0, 0, payload)), limits, zipcheck.ErrNested},
		"descriptor, sizes given":                   {pdfStream(141, localHeader(8, 8, uint32(len(payload)), 4000, payload)), limits, zipcheck.ErrNested},
		"descriptor, stored":                        {pdfStream(141, localHeader(8, 0, 4, 4, text(200))), limits, zipcheck.ErrNested},
		"zero sizes":                                {pdfStream(141, localHeader(0, 8, 0, 0, payload)), limits, zipcheck.ErrNested},
		"zero usize":                                {pdfStream(141, localHeader(0, 8, uint32(len(payload)), 0, payload)), limits, zipcheck.ErrNested},
		"csize short of the stream":                 {pdfStream(141, localHeader(0, 8, 64, 4000, payload)), limits, zipcheck.ErrNested},
		"csize cuts before a byte":                  {pdfStream(141, localHeader(0, 8, 2, 4000, payload)), limits, zipcheck.ErrNested},
		"usize short of the stream":                 {pdfStream(141, localHeader(0, 8, uint32(len(payload)), 100, payload)), limits, zipcheck.ErrNested},
		"usize past the stream":                     {pdfStream(141, localHeader(0, 8, uint32(len(payload)), 5000, payload)), limits, zipcheck.ErrNested},
		"broken after its data":                     {pdfStream(141, localHeader(0, 8, uint32(len(brokenAfter(t, text(1000)))), 1000, brokenAfter(t, text(1000)))), limits, zipcheck.ErrNested},
		"stored, usize past csize":                  {pdfStream(141, localHeader(0, 0, uint32(len(bomb)), 2<<20, bomb)), limits, zipcheck.ErrNested},
		"stored, usize short of csize":              {pdfStream(141, localHeader(0, 0, 200, 100, text(200))), limits, zipcheck.ErrNested},
		"encrypted":                                 {pdfStream(141, localHeader(1, 8, 64, 1000, text(200))), limits, zipcheck.ErrNested},
		"bzip2":                                     {pdfStream(141, localHeader(0, 12, 64, 1000, text(200))), limits, zipcheck.ErrNested},
		"past MaxFileSize":                          {pdfStream(141, valid(8, zeros(2<<20))), limits, zipcheck.ErrTooLarge},
		"past MaxFiles":                             {pdfStream(141, bytes.Repeat(valid(0, []byte("abcd")), 3)), tight, zipcheck.ErrTooLarge},
		"a ZIP mid-PDF past MaxFileSize":            {pdfStream(141, big), limits, zipcheck.ErrTooLarge},
		"a ZIP mid-PDF past MaxFileSize, 70 KiB on": {pdfStream(141, append(append([]byte{}, big...), padding(70<<10)...)), limits, zipcheck.ErrTooLarge},
		"a ZIP mid-PDF holding a RAR":               {pdfStream(141, rar), limits, zipcheck.ErrNested},
		"a ZIP mid-PDF holding a large gzip":        {pdfStream(141, gz), limits, zipcheck.ErrTooLarge},
		"a ZIP mid-PDF with descriptors":            {pdfStream(141, streamed), limits, zipcheck.ErrNested},
		"a ZIP mid-PDF with descriptors, 70 KiB on": {pdfStream(141, append(append([]byte{}, streamed...), padding(70<<10)...)), limits, zipcheck.ErrNested},
		"a ZIP appended with descriptors":           {append(pdf(), streamed...), limits, zipcheck.ErrNested},
		"an end record, its directory elsewhere": {
			append(append(append(pdf(), valid(0, []byte("abcd"))...), small...), padding(100)...), limits, zipcheck.ErrNested,
		},
		"an end record past the file": {append(pdf(), overrun...), limits, zipcheck.ErrNested},
	} {
		t.Run(name, func(t *testing.T) {
			if err := checkFile(t, c.data, c.limits); !errors.Is(err, c.want) {
				t.Fatalf("err = %v, want %v", err, c.want)
			}
		})
	}
}
