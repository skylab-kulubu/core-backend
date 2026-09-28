package zipcheck_test

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/skylab-kulubu/core-backend/internal/clamd"
	"github.com/skylab-kulubu/core-backend/internal/clamd/clamdtest"
	"github.com/skylab-kulubu/core-backend/internal/zipcheck"
)

// realClamAV is a real clamd 1.5.4 with the test's limits (MaxFileSize 1M,
// MaxScanSize 4M, AlertExceedsMax on as the wizard sets it) and the test
// marker signature (clamdtest.Marker), which clamd reports anywhere in what
// it reads.
func realClamAV(t *testing.T) (*clamd.Client, context.Context) {
	t.Helper()
	addr := clamdtest.RealWithMarker(t, "CLAMD_CONF_MaxFileSize=1M", "CLAMD_CONF_MaxScanSize=4M",
		"CLAMD_CONF_StreamMaxLength=16M", "CLAMD_CONF_AlertExceedsMax=yes")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	t.Cleanup(cancel)
	return clamd.New(addr), ctx
}

// marked is n bytes of padding with the test marker at its end: in a
// member past MaxFileSize, the part clamd does not read.
func marked(n int) []byte {
	return append(bytes.Repeat([]byte(" "), n), clamdtest.Marker...)
}

// scanned asserts what clamd answers for data: the signature found, or ""
// for clean. A file that carries the marker must not carry it raw (clamd
// reads a whole file raw as well): only unpacked can it be found.
func scanned(t *testing.T, client *clamd.Client, ctx context.Context, name string, data []byte, want string) {
	t.Helper()
	if bytes.Contains(data, []byte(clamdtest.Marker)) {
		t.Fatalf("%s carries the marker raw", name)
	}
	verdict, err := client.Scan(ctx, bytes.NewReader(data))
	if err != nil || verdict.Signature != want {
		t.Errorf("%s: clamd answered %+v, %v; want %q", name, verdict, err, want)
	}
}

// clamd reads only the first MaxFileSize bytes of a member past it, without
// a report: the marker at the start of 2 MiB is found, at its end it is not.
func TestRealClamAVReadsAMemberOnlyUpToMaxFileSize(t *testing.T) {
	client, ctx := realClamAV(t)
	first := append([]byte(clamdtest.Marker), bytes.Repeat([]byte(" "), 2<<20)...)
	scanned(t, client, ctx, "the marker first", build(t, deflated("m.txt", first)), clamdtest.MarkerSignature)
	scanned(t, client, ctx, "the marker last", build(t, deflated("m.txt", marked(2<<20))), "")
}

// Each file below carries a marker, or the EICAR test file as a whole
// member, where clamd 1.5.4 does not read it: clamd answers it clean. The
// check refuses every one. Among them: a DOCX whose part inflates past
// MaxFileSize, a ZIP appended to a stub (a self-extracting program) or to a
// PDF, and a lone local header hidden in a ZIP's end record comment or a
// member's extra field, which clamd unpacks where it finds it and reads only
// up to MaxFileSize.
func TestRealClamAVPassesUnscannedWhatTheCheckRefuses(t *testing.T) {
	client, ctx := realClamAV(t)
	eicar := clamd.EICAR()

	lying := build(t, file{name: "m.txt", data: marked(2 << 20), method: zip.Deflate, sized: true})
	l := layoutOf(t, lying)
	put32(lying, l.dir[0]+dirUSize, 100)
	put32(lying, l.local[0]+locUSize, 100)
	unknownMethod := build(t, file{name: "e.txt", data: eicar, method: zip.Store, sized: true})
	l = layoutOf(t, unknownMethod)
	put16(unknownMethod, l.dir[0]+dirMethod, 99)
	put16(unknownMethod, l.local[0]+locMethod, 99)
	hiddenPastMax := lone(20, 0, 8, "h.txt", marked(2<<20))

	for name, c := range map[string]struct {
		data []byte
		want error
	}{
		"a member past MaxFileSize":              {build(t, deflated("m.txt", marked(2<<20))), zipcheck.ErrTooLarge},
		"a member whose headers say 100 bytes":   {lying, zipcheck.ErrInvalid},
		"a member with ZIP64 sizes":              {zip64Member(eicar, uint64(len(eicar))), zipcheck.ErrInvalid},
		"a member with an unknown method":        {unknownMethod, zipcheck.ErrInvalid},
		"a nested ZIP's member past MaxFileSize": {build(t, deflated("inner.zip", build(t, deflated("m.txt", marked(2<<20))))), zipcheck.ErrTooLarge},
		"a DOCX whose part inflates past MaxFileSize": {
			office(t, "word/document.xml", deflated("word/media/image1.bin", marked(2<<20))), zipcheck.ErrTooLarge,
		},
		"a ZIP after a stub whose member inflates past MaxFileSize": {
			build(t, deflated("setup.exe", sfx(t, stub(4096), build(t, sized(deflated("m.txt", marked(2<<20)))...), false))), zipcheck.ErrTooLarge,
		},
		"a PDF with a ZIP appended whose member inflates past MaxFileSize": {
			append(pdf(), build(t, sized(deflated("m.txt", marked(2<<20)))...)...), zipcheck.ErrTooLarge,
		},
		"a ZIP hiding a member past MaxFileSize in its comment": {zipCommented(t, hiddenPastMax), zipcheck.ErrInvalid},
		"a ZIP hiding a member past MaxFileSize in an extra":    {zipWithExtra(t, extraField(hiddenPastMax), ""), zipcheck.ErrInvalid},
		"a PDF with a ZIP hiding a member past MaxFileSize in its comment": {
			append(pdf(), zipCommented(t, hiddenPastMax)...), zipcheck.ErrInvalid,
		},
	} {
		scanned(t, client, ctx, name, c.data, "")
		if err := checkFile(t, c.data, limits); !errors.Is(err, c.want) {
			t.Errorf("%s: check err = %v, want %v", name, err, c.want)
		}
	}
}

// clamd unpacks a lone local header wherever it finds one: hidden in a
// ZIP's end record comment or a member's extra field, in a ZIP or in a ZIP
// appended to a PDF, the marker in a small hidden member is found. No
// directory lists it, so the check refuses them all, as it refuses the
// large ones clamd reads only in part.
func TestRealClamAVUnpacksHiddenLocalHeaders(t *testing.T) {
	client, ctx := realClamAV(t)
	small := lone(20, 0, 8, "h.txt", append([]byte(clamdtest.Marker), bytes.Repeat([]byte(" "), 100)...))
	for name, data := range map[string][]byte{
		"in a ZIP's comment":             zipCommented(t, small),
		"in a member's extra field":      zipWithExtra(t, extraField(small), ""),
		"in a comment, the ZIP in a PDF": append(pdf(), zipCommented(t, small)...),
		"in an extra, the ZIP in a PDF":  append(pdf(), zipWithExtra(t, extraField(small), "")...),
		"appended to a PDF, version 255": append(pdf(), lone(255, 0, 8, "h.txt", append([]byte(clamdtest.Marker), bytes.Repeat([]byte(" "), 100)...))...),
	} {
		scanned(t, client, ctx, name, data, clamdtest.MarkerSignature)
		if err := checkFile(t, data, limits); err == nil {
			t.Errorf("%s: the check passed it", name)
		}
	}
}

// Each file the check passes is scanned whole: clamd finds the EICAR test
// file, a whole member, in it: in a nested ZIP, three levels deep, a gzip, a
// tar.gz, a DOCX, a ZIP appended to a stub and a ZIP appended to a PDF.
func TestRealClamAVScansWhatTheCheckPasses(t *testing.T) {
	client, ctx := realClamAV(t)
	eicar := clamd.EICAR()
	for name, data := range map[string][]byte{
		"a ZIP":                          build(t, deflated("e.txt", eicar), deflated("inner.zip", build(t, deflated("f.txt", eicar)))),
		"a nested ZIP":                   build(t, deflated("inner.zip", build(t, deflated("f.txt", eicar)))),
		"a ZIP three levels deep":        build(t, deflated("a.zip", build(t, deflated("b.zip", build(t, deflated("c.zip", build(t, deflated("f.txt", eicar)))))))),
		"a gzip":                         build(t, stored("e.txt.gz", gzOf(t, eicar))),
		"a tar.gz":                       build(t, stored("e.tgz", gzOf(t, tarOf(t, false, stored("e.txt", eicar))))),
		"a DOCX":                         office(t, "word/document.xml", deflated("word/media/image1.bin", eicar)),
		"a ZIP after a stub in a member": build(t, deflated("setup.exe", sfx(t, stub(4096), build(t, sized(deflated("e.txt", eicar))...), false)), filler()),
		"a PDF with a ZIP appended":      append(pdf(), build(t, sized(deflated("e.txt", eicar))...)...),
	} {
		if err := checkFile(t, data, limits); err != nil {
			t.Errorf("%s: refused: %v", name, err)
		}
		if verdict, err := client.Scan(ctx, bytes.NewReader(data)); err != nil || verdict.Signature != clamdtest.Signature {
			t.Errorf("%s the check passes: clamd answered %+v, %v", name, verdict, err)
		}
	}
}

// Where a ZIP holds another end record signature inside a member's data (a
// PPTX of images deflated into stored blocks, a stored inner ZIP, what
// Info-ZIP writes when it stores one) clamd 1.5.4 reads the ZIP's own, last
// end record: it finds the EICAR test file in the ZIP's first member, which
// only the outer directory lists. The check passes these ZIPs, reading the
// same directory.
func TestRealClamAVReadsTheLastEndRecord(t *testing.T) {
	client, ctx := realClamAV(t)
	eicar := deflated("e.txt", clamd.EICAR())
	images := build(t, stored("ppt/media/image1.jpeg", text(20<<10)))
	inner := build(t, deflated("b.txt", []byte("inner")))
	for name, data := range map[string][]byte{
		"a ZIP ending with a PPTX of images":   build(t, eicar, deflated("sunum.pptx", images)),
		"a small ZIP with a stored inner ZIP":  build(t, eicar, stored("inner.zip", inner)),
		"a ZIP ending with a stored empty ZIP": build(t, eicar, stored("empty.zip", build(t))),
		"as Info-ZIP stores an inner ZIP":      build(t, sized(eicar, stored("inner.zip", build(t, sized(stored("photo.jpg", text(6000)))...)))...),
	} {
		if n := bytes.Count(data, []byte("PK\x05\x06")); n < 2 {
			t.Fatalf("%s holds %d end record signatures, want another besides its own", name, n)
		}
		if verdict, err := client.Scan(ctx, bytes.NewReader(data)); err != nil || verdict.Signature != clamdtest.Signature {
			t.Errorf("%s: clamd answered %+v, %v; it did not read the ZIP's own directory", name, verdict, err)
		}
		passes(t, data, limits)
	}
}
