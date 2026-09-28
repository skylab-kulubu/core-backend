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

// Against a real clamd 1.5.4 with the test's limits (MaxFileSize 1M,
// MaxScanSize 4M, AlertExceedsMax on as the wizard sets it), each file below
// carries the EICAR test file where clamd does not find it: clamd answers it
// clean. The check refuses every one. Among them: a DOCX whose part inflates
// past MaxFileSize, and a ZIP appended to a stub (a self-extracting program)
// inside a member, which clamd unpacks from past the member's first byte and
// then skips. Each file the check passes is scanned whole: clamd finds the
// EICAR test file in it, in a nested ZIP, a gzip, a tar.gz, a DOCX and a ZIP
// appended to a stub alike.
func TestRealClamAVPassesUnscannedWhatTheCheckRefuses(t *testing.T) {
	addr := clamdtest.Real(t, "CLAMD_CONF_MaxFileSize=1M", "CLAMD_CONF_MaxScanSize=4M",
		"CLAMD_CONF_StreamMaxLength=16M", "CLAMD_CONF_AlertExceedsMax=yes")
	client := clamd.New(addr)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	eicar := clamd.EICAR()
	padded := append(append([]byte{}, eicar...), bytes.Repeat([]byte(" "), 2<<20)...)

	lying := build(t, file{name: "e.txt", data: padded, method: zip.Deflate, sized: true})
	l := layoutOf(t, lying)
	put32(lying, l.dir[0]+dirUSize, 100)
	put32(lying, l.local[0]+locUSize, 100)
	unknownMethod := build(t, file{name: "e.txt", data: eicar, method: zip.Store, sized: true})
	l = layoutOf(t, unknownMethod)
	put16(unknownMethod, l.dir[0]+dirMethod, 99)
	put16(unknownMethod, l.local[0]+locMethod, 99)

	for name, c := range map[string]struct {
		data []byte
		want error
	}{
		"a member past MaxFileSize":              {build(t, deflated("e.txt", padded)), zipcheck.ErrTooLarge},
		"a member whose headers say 100 bytes":   {lying, zipcheck.ErrInvalid},
		"a member with ZIP64 sizes":              {zip64Member(eicar, uint64(len(eicar))), zipcheck.ErrInvalid},
		"a member with an unknown method":        {unknownMethod, zipcheck.ErrInvalid},
		"a nested ZIP's member past MaxFileSize": {build(t, deflated("inner.zip", build(t, deflated("e.txt", padded)))), zipcheck.ErrTooLarge},
		"a DOCX whose part inflates past MaxFileSize": {
			office(t, "word/document.xml", deflated("word/media/image1.bin", padded)), zipcheck.ErrTooLarge,
		},
		"a PDF with a ZIP appended whose member inflates past MaxFileSize": {
			append(pdf(), build(t, sized(deflated("e.txt", padded))...)...), zipcheck.ErrTooLarge,
		},
		"a ZIP after a stub whose member inflates past MaxFileSize": {
			build(t, deflated("setup.exe", sfx(t, stub(4096), build(t, sized(deflated("e.txt", padded))...), false))), zipcheck.ErrTooLarge,
		},
	} {
		verdict, err := client.Scan(ctx, bytes.NewReader(c.data))
		if err != nil || verdict.Infected() {
			t.Errorf("%s: clamd answered %+v, %v; the check was built on clamd passing it unscanned", name, verdict, err)
		}
		if err := checkFile(t, c.data, limits); !errors.Is(err, c.want) {
			t.Errorf("%s: check err = %v, want %v", name, err, c.want)
		}
	}

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
	addr := clamdtest.Real(t, "CLAMD_CONF_MaxFileSize=1M", "CLAMD_CONF_MaxScanSize=4M",
		"CLAMD_CONF_StreamMaxLength=16M", "CLAMD_CONF_AlertExceedsMax=yes")
	client := clamd.New(addr)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
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
