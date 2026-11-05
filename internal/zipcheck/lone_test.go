package zipcheck_test

import (
	"bytes"
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

// A file that is no ZIP, with a local header signature past its first byte
// and no end record at its end, holds no ZIP a reader opens from its end;
// clamd unpacks each local header in it on its own (checked: a lone one
// appended to a PDF). Production refused a PDF with one at byte 141 as
// archive_nested (no ZIP ends the file there), every try. Each local header
// there is now a lone member, checked as clamd unpacks it: one that holds
// nothing past the limits passes, and the file goes to clamd whole.
func TestCheckFileChecksLoneLocalHeadersInAFile(t *testing.T) {
	t.Parallel()
	odf := build(t, sized(stored("mimetype", []byte("application/vnd.oasis.opendocument.text")),
		deflated("content.xml", text(20<<10)), deflated("styles.xml", bytes.Repeat([]byte("<style/>"), 1000)))...)
	streamed := build(t, deflated("content.xml", text(20<<10)), deflated("styles.xml", bytes.Repeat([]byte("<style/>"), 1000)))
	for name, data := range map[string][]byte{
		// The signature, then data no deflate stream starts with: clamd
		// inflates nothing from it.
		"at byte 141, deflated, then bytes no inflater reads": pdfStream(141, localHeader(0, 8, 64, 1000, append([]byte{0xff}, text(200)...))),
		"at byte 141, deflated, then binary data":             pdfStream(141, localHeader(0, 8, 64, 1000, text(4000))),
		"at byte 141, deflated, the stream cut short":         pdfStream(141, localHeader(0, 8, 10, 1000, deflate(t, text(4000))[:10])),
		"at byte 141, stored, over the rest of the PDF":       pdfStream(141, localHeader(0, 0, 3000, 3000, text(200))),
		"at byte 141, stored, sizes that say it grows":        pdfStream(141, localHeader(0, 0, 100, 5000, text(200))),
		"at byte 141, deflated, its size in a descriptor":     pdfStream(141, localHeader(8, 8, 0, 0, deflate(t, text(4000)))),
		"a reserved flag":                                      pdfStream(141, localHeader(1<<15, 8, 64, 1000, text(200))),
		"a whole ZIP in its middle (a hybrid PDF)":             pdfStream(141, odf),
		"a whole ZIP in its middle, streamed with descriptors": pdfStream(141, streamed),
		"a whole ZIP in the middle of a member":                build(t, deflated("rapor.pdf", pdfStream(141, odf))),
	} {
		t.Run(name, func(t *testing.T) {
			if err := checkFile(t, data, limits); err != nil {
				t.Fatalf("refused: %v", err)
			}
		})
	}
}

// What a lone member unpacks to is held to the limits as any member's:
// clamd reads only up to MaxFileSize of it. One core cannot measure (an
// encrypted one, another method, a stored one whose size it cannot tell) is
// refused as nested. A file that ends with an end record whose directory
// is elsewhere is no file of lone members: refused as nested, as before.
func TestCheckFileRefusesLoneMembersPastTheLimits(t *testing.T) {
	t.Parallel()
	bomb := deflate(t, zeros(2<<20))
	big := build(t, sized(stored("mimetype", []byte("application/vnd.oasis.opendocument.text")), deflated("zeros.bin", zeros(2<<20)))...)
	streamedBig := build(t, deflated("zeros.bin", zeros(2<<20)))
	small := build(t, sized(deflated("notlar.txt", []byte("ek notlar")))...)
	rar := build(t, sized(stored("a.rar", append([]byte("Rar!\x1a\x07\x00"), text(100)...)))...)
	nested := build(t, sized(deflated("a.gz", gzOf(t, zeros(2<<20))))...)
	tight := limits
	tight.MaxFiles = 2
	for name, c := range map[string]struct {
		data   []byte
		limits zipcheck.Limits
		want   error
	}{
		"deflated past MaxFileSize":                         {pdfStream(141, localHeader(0, 8, uint32(len(bomb)), 2<<20, bomb)), limits, zipcheck.ErrTooLarge},
		"its csize cutting a stream that runs past it":      {pdfStream(141, localHeader(0, 8, 64, 100, bomb)), limits, zipcheck.ErrTooLarge},
		"its size in a descriptor, past MaxFileSize":        {pdfStream(141, localHeader(8, 8, 0, 0, bomb)), limits, zipcheck.ErrTooLarge},
		"stored, sizes that say it grows past MaxFileSize":  {pdfStream(141, localHeader(0, 0, uint32(len(bomb)), 2<<20, bomb)), limits, zipcheck.ErrTooLarge},
		"a ZIP in its middle past MaxFileSize":              {pdfStream(141, big), limits, zipcheck.ErrTooLarge},
		"a streamed ZIP in its middle past MaxFileSize":     {pdfStream(141, streamedBig), limits, zipcheck.ErrTooLarge},
		"a ZIP in its middle holding a RAR":                 {pdfStream(141, rar), limits, zipcheck.ErrNested},
		"a ZIP in its middle holding a gzip past the limit": {pdfStream(141, nested), limits, zipcheck.ErrTooLarge},
		"more lone members than MaxFiles":                   {pdfStream(141, bytes.Repeat(localHeader(0, 0, 4, 4, []byte("abcd")), 3)), tight, zipcheck.ErrTooLarge},
		"encrypted":                                         {pdfStream(141, localHeader(1, 8, 64, 1000, text(200))), limits, zipcheck.ErrNested},
		"compressed with bzip2":                             {pdfStream(141, localHeader(0, 12, 64, 1000, text(200))), limits, zipcheck.ErrNested},
		"stored, its size unknown":                          {pdfStream(141, localHeader(8, 0, 0, 0, text(200))), limits, zipcheck.ErrNested},
		"stored, its size in a descriptor too":              {pdfStream(141, localHeader(8, 0, 4, 4, text(200))), limits, zipcheck.ErrNested},
		"stored, its data a deflate past MaxFileSize":       {pdfStream(141, localHeader(0, 0, 64, 64, bomb)), limits, zipcheck.ErrTooLarge},
		"one, then a ZIP appended whose directory counts from it": {
			append(pdfStream(141, localHeader(0, 8, 64, 1000, append([]byte{0xff}, text(200)...))), small...), limits, zipcheck.ErrNested,
		},
	} {
		t.Run(name, func(t *testing.T) {
			if err := checkFile(t, c.data, c.limits); !errors.Is(err, c.want) {
				t.Fatalf("err = %v, want %v", err, c.want)
			}
		})
	}
}
