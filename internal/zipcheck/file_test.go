package zipcheck_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

	"github.com/skylab-kulubu/core-backend/internal/zipcheck"
)

// pdf, jpeg and png are small files of those kinds, as uploads are.
func pdf() []byte {
	return append([]byte("%PDF-1.7\n1 0 obj << /Type /Catalog >> endobj\n"), append(text(3000), "\ntrailer << >>\n%%EOF\n"...)...)
}

func jpeg() []byte {
	return append([]byte{0xff, 0xd8, 0xff, 0xe0, 0, 16, 'J', 'F', 'I', 'F', 0}, append(text(3000), 0xff, 0xd9)...)
}

func png() []byte {
	return append([]byte("\x89PNG\r\n\x1a\n\x00\x00\x00\x0dIHDR"), append(text(3000), "\x00\x00\x00\x00IEND\xaeB`\x82"...)...)
}

// checkFile checks data as any scanned file, both ways the scan reads one:
// by ranges (a public held file) and as a stream (a private file,
// decrypted as it is read). Both must agree.
func checkFile(t *testing.T, data []byte, l zipcheck.Limits) error {
	t.Helper()
	ranged := zipcheck.CheckFile(context.Background(), &source{data: data}, int64(len(data)), l)
	streamed := zipcheck.CheckStream(context.Background(), bytes.NewReader(data), int64(len(data)), l)
	for _, want := range []error{zipcheck.ErrTooLarge, zipcheck.ErrInvalid, zipcheck.ErrNested, nil} {
		if (want == nil && (ranged == nil) != (streamed == nil)) || (want != nil && errors.Is(ranged, want) != errors.Is(streamed, want)) {
			t.Fatalf("read by ranges: %v; as a stream: %v", ranged, streamed)
		}
	}
	return ranged
}

// Every scanned file is read once before clamd, whatever it is: clamd
// unpacks an archive it finds past the first byte of a PDF or an image too
// (checked: a ZIP appended to a PDF), and skips there what it skips in a ZIP.
// So a ZIP appended to a file is checked as a nested one, ending the file,
// its offsets counted from the ZIP or from the file's first byte; one past
// clamd's limits is refused as too large, and one that does not end the
// file, or any other archive there, as nested. A PDF, JPEG or PNG with
// nothing appended, or with a ZIP within the limits, passes.
func TestCheckFileLooksForArchivesInAnyFile(t *testing.T) {
	t.Parallel()
	small := build(t, sized(deflated("notlar.txt", []byte("ek notlar")))...)
	big := build(t, sized(deflated("zeros.bin", zeros(2<<20)))...)
	for name, data := range map[string][]byte{
		"a PDF":                            pdf(),
		"a JPEG":                           jpeg(),
		"a PNG":                            png(),
		"an empty file":                    nil,
		"a PDF with a ZIP appended":        append(pdf(), small...),
		"a PDF with a ZIP counted from it": sfx(t, pdf(), small, true),
		"a JPEG with a ZIP appended":       append(jpeg(), small...),
		"a ZIP":                            small,
	} {
		t.Run(name, func(t *testing.T) {
			if err := checkFile(t, data, limits); err != nil {
				t.Fatalf("refused: %v", err)
			}
		})
	}
	for name, c := range map[string]struct {
		data []byte
		want error
	}{
		"a PDF with a ZIP past MaxFileSize":      {append(pdf(), big...), zipcheck.ErrTooLarge},
		"a PNG with a ZIP past MaxFileSize":      {append(png(), big...), zipcheck.ErrTooLarge},
		"offsets from the PDF, past MaxFileSize": {sfx(t, pdf(), big, true), zipcheck.ErrTooLarge},
		"a ZIP in the middle of a PDF":           {append(append(pdf(), small...), pdf()...), zipcheck.ErrNested},
		"a RAR appended to a PDF":                {append(pdf(), append([]byte("Rar!\x1a\x07\x00"), text(100)...)...), zipcheck.ErrNested},
		"a 7-Zip appended to a JPEG":             {append(jpeg(), append([]byte("7z\xbc\xaf\x27\x1c"), text(100)...)...), zipcheck.ErrNested},
		"a cab in a PNG":                         {append(png(), append([]byte("MSCF\x00\x00\x00\x00"), text(100)...)...), zipcheck.ErrNested},
		"an ARJ appended to a PDF":               {append(pdf(), append(arjHeader(), text(100)...)...), zipcheck.ErrNested},
		"a ZIP past MaxFileSize":                 {big, zipcheck.ErrTooLarge},
	} {
		t.Run(name, func(t *testing.T) {
			if err := checkFile(t, c.data, limits); !errors.Is(err, c.want) {
				t.Fatalf("err = %v, want %v", err, c.want)
			}
		})
	}
}

// A file read as a stream (a private file, decrypted as it is read) has no
// ranges to read again: a ZIP is kept in memory to be checked, at most
// MaxBuffer. A larger ZIP is ErrZIPTooLargeToKeep, which says nothing about
// it; a larger ZIP appended to another file is refused as nested. A file
// read by ranges has neither limit.
func TestCheckStreamKeepsAZIPInMemory(t *testing.T) {
	t.Parallel()
	little := limits
	little.MaxBuffer = 16 << 10
	archive := build(t, sized(stored("a.bin", text(20<<10)))...)
	ctx := context.Background()
	if err := zipcheck.CheckStream(ctx, bytes.NewReader(archive), int64(len(archive)), little); !errors.Is(err, zipcheck.ErrZIPTooLargeToKeep) || isRefusal(err) {
		t.Fatalf("a ZIP larger than MaxBuffer: err = %v, want %v", err, zipcheck.ErrZIPTooLargeToKeep)
	}
	appended := append(pdf(), archive...)
	if err := zipcheck.CheckStream(ctx, bytes.NewReader(appended), int64(len(appended)), little); !errors.Is(err, zipcheck.ErrNested) {
		t.Fatalf("a ZIP larger than MaxBuffer appended to a PDF: err = %v, want %v", err, zipcheck.ErrNested)
	}
	for _, data := range [][]byte{archive, appended} {
		if err := zipcheck.CheckFile(ctx, &source{data: data}, int64(len(data)), little); err != nil {
			t.Fatalf("read by ranges: %v", err)
		}
	}
	// A stream that ends before its size is no refusal either.
	if err := zipcheck.CheckStream(ctx, io.LimitReader(bytes.NewReader(appended), 1000), int64(len(appended)), limits); err == nil || isRefusal(err) {
		t.Fatalf("a stream cut short: err = %v", err)
	}
}

func isRefusal(err error) bool {
	var r *zipcheck.Refusal
	return errors.As(err, &r)
}
