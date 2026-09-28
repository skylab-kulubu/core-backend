package zipcheck_test

import (
	"archive/zip"
	"bytes"
	"fmt"
	"testing"

	"github.com/skylab-kulubu/core-backend/internal/zipcheck"
)

// Archives are told by their content, never by their names: a disk image,
// or a file named as a 7-Zip, ZIP or tar.gz archive, holding plain bytes is
// a plain file.
func TestCheckTellsArchivesByContentNotByName(t *testing.T) {
	t.Parallel()
	passes(t, build(t,
		stored("raspi.img", text(4000)),
		stored("backup.7z", text(100)),
		deflated("not-really.zip", bytes.Repeat([]byte("zip "), 100)),
		stored("veri.tar.gz", text(100)),
		stored("a.RAR", text(100)),
	), limits)
}

// A ZIP inside the ZIP, an Office package among them, is checked the same
// way, and so is one inside that, down to min(MaxRecursion-2, 3) archives
// deep: clamd holds what is inside it to the same limits (checked). Its
// members count toward the same limits. Deeper nesting, or any while
// MaxRecursion is too low for clamd to reach its members, is refused as
// nested: unpack it and upload again.
func TestCheckNestedZIPs(t *testing.T) {
	t.Parallel()
	inner := build(t, deflated("a.txt", bytes.Repeat([]byte("a"), 4000)), stored("b.bin", text(300)))
	// nest is a ZIP holding a ZIP holding … depth ZIPs deep, the last one
	// inner.
	nest := func(depth int, member func(string, []byte) file) []byte {
		data := inner
		for i := range depth {
			data = build(t, member(fmt.Sprintf("level-%d.zip", i), data), filler())
		}
		return data
	}
	workbook := func(parts ...file) []byte { return office(t, "xl/workbook.xml", parts...) }
	slides := func(embedded []byte) []byte {
		return office(t, "ppt/presentation.xml", deflated("ppt/embeddings/Microsoft_Excel_Worksheet.xlsx", embedded))
	}
	for name, data := range map[string][]byte{
		"a deflated ZIP":                      nest(1, deflated),
		"a stored ZIP":                        build(t, stored("inner.zip", inner), deflated("z.txt", []byte("z")), filler()),
		"three levels deep, deflated":         nest(3, deflated),
		"three levels deep, stored":           nest(3, stored),
		"a PPTX with an embedded workbook":    build(t, deflated("sunum.pptx", slides(workbook(deflated("xl/sheet1.xml", []byte("<sheet/>")))))),
		"a DOCX and an XLSX":                  build(t, deflated("rapor.docx", office(t, "word/document.xml")), deflated("tablo.xlsx", workbook())),
		"an empty ZIP":                        build(t, stored("empty.zip", build(t)), filler()),
		"an empty member named as an archive": build(t, stored("empty.zip", nil)),
	} {
		t.Run(name, func(t *testing.T) {
			passes(t, data, limits)
		})
	}

	lyingInner := build(t, file{name: "zeros.bin", data: zeros(300 << 10), method: zip.Deflate, sized: true})
	l := layoutOf(t, lyingInner)
	put32(lyingInner, l.dir[0]+dirUSize, 100)
	put32(lyingInner, l.local[0]+locUSize, 100)
	withRecursion := func(n int) zipcheck.Limits {
		lim := limits
		lim.MaxRecursion = n
		return lim
	}
	fewFiles := limits
	fewFiles.MaxFiles = 3
	for name, c := range map[string]struct {
		data   []byte
		limits zipcheck.Limits
		want   error
	}{
		"four levels deep":                   {nest(4, deflated), limits, zipcheck.ErrNested},
		"four levels deep, stored":           {nest(4, stored), limits, zipcheck.ErrNested},
		"a ZIP where MaxRecursion is 2":      {nest(1, deflated), withRecursion(2), zipcheck.ErrNested},
		"two levels where MaxRecursion is 3": {nest(2, deflated), withRecursion(3), zipcheck.ErrNested},
		"a nested member past MaxFileSize":   {build(t, deflated("inner.zip", build(t, deflated("zeros.bin", zeros(2<<20))))), limits, zipcheck.ErrTooLarge},
		"an embedded workbook's member past MaxFileSize": {
			build(t, deflated("sunum.pptx", slides(workbook(deflated("xl/media/zeros.bin", zeros(2<<20)))))), limits, zipcheck.ErrTooLarge,
		},
		"a nested member that lies":             {build(t, deflated("inner.zip", lyingInner)), limits, zipcheck.ErrInvalid},
		"a nested ZIP with bytes after its end": {build(t, deflated("inner.zip", append(append([]byte{}, inner...), "tail"...))), limits, zipcheck.ErrInvalid},
		"more members than MaxFiles in all":     {build(t, deflated("inner.zip", inner), stored("c.txt", nil)), fewFiles, zipcheck.ErrTooLarge},
	} {
		t.Run(name, func(t *testing.T) {
			refused(t, c.data, c.limits, c.want)
		})
	}
}

// A ZIP inside that is deflated, or sits in a gzip or tar stream, is read
// again from memory: it is kept there when it is at most MaxBuffer. A
// larger one is refused as nested. A stored one is read again from the file
// it lies in, whatever its size.
func TestCheckKeepsOnlySmallInnerZIPsInMemory(t *testing.T) {
	t.Parallel()
	small := limits
	small.MaxBuffer = 64 << 10
	large := build(t, stored("a.bin", text(80<<10)), stored("b.bin", text(10<<10)))
	fits := build(t, stored("a.bin", text(30<<10)))
	refused(t, build(t, deflated("inner.zip", large)), small, zipcheck.ErrNested)
	refused(t, build(t, deflated("data.tar", tarOf(t, false, stored("inner.zip", large)))), small, zipcheck.ErrNested)
	passes(t, build(t, stored("inner.zip", large), filler()), small)
	passes(t, build(t, deflated("inner.zip", fits), filler()), small)
}

// A nested ZIP larger than 8 MiB is read whole all the same, from memory or
// from the file.
func TestCheckReadsALargeNestedZIP(t *testing.T) {
	t.Parallel()
	inner := build(t, stored("a.bin", text(5<<20)), deflated("b.txt", bytes.Repeat([]byte("b"), 4<<20)), stored("c.bin", text(1<<20)))
	roomy := zipcheck.Limits{MaxFileSize: 16 << 20, MaxScanSize: 64 << 20, MaxFiles: 100, MaxRecursion: 17, MaxBuffer: 64 << 20}
	passes(t, build(t, deflated("inner.zip", inner)), roomy)
	passes(t, build(t, stored("inner.zip", inner), filler()), roomy)
}

// gzip, bzip2 and tar are opened as clamd opens them: what a gzip or bzip2
// member unpacks to is measured against MaxFileSize and MaxScanSize, and
// every tar entry is a member of its own, counted toward MaxFiles. What
// they hold is read in turn, a ZIP included. One that is corrupt is
// invalid; a gzip of more than one member, whose later ones some readers
// skip, is refused as nested.
func TestCheckOpensGzipBzip2AndTar(t *testing.T) {
	t.Parallel()
	csv := []byte("ad,soyad\nAda,Lovelace\n")
	zipOf := func(files ...file) []byte { return build(t, files...) }
	for name, data := range map[string][]byte{
		"a gzip":             build(t, stored("data.csv.gz", gzOf(t, csv))),
		"a bzip2":            build(t, stored("data.csv.bz2", bzip2CSV)),
		"a tar":              build(t, deflated("data.tar", tarOf(t, false, stored("a.csv", csv), stored("b.csv", csv)))),
		"a tar.gz":           build(t, stored("data.tar.gz", gzOf(t, tarOf(t, false, stored("a.csv", csv))))),
		"an old tar":         build(t, deflated("data", tarOf(t, true, stored("a.csv", csv)))),
		"a ZIP in a tar":     build(t, deflated("data.tar", tarOf(t, false, stored("inner.zip", zipOf(deflated("a.csv", csv)))))),
		"a ZIP in a gzip":    build(t, stored("inner.zip.gz", gzOf(t, zipOf(deflated("a.csv", csv))))),
		"a deflated tar.gz":  build(t, deflated("data.tgz", gzOf(t, tarOf(t, false, stored("a.csv", csv))))),
		"an empty tar entry": build(t, deflated("data.tar", tarOf(t, false, stored("empty", nil)))),
	} {
		t.Run(name, func(t *testing.T) {
			passes(t, data, limits)
		})
	}

	fewFiles := limits
	fewFiles.MaxFiles = 3
	badCRC := gzOf(t, csv)
	badCRC[len(badCRC)-8] ^= 1
	badBzip2 := append([]byte{}, bzip2CSV...)
	badBzip2[20] ^= 0xff
	for name, c := range map[string]struct {
		data   []byte
		limits zipcheck.Limits
		want   error
	}{
		"a gzip past MaxFileSize":             {build(t, stored("zeros.gz", gzOf(t, zeros(2<<20)))), limits, zipcheck.ErrTooLarge},
		"a bzip2 past MaxFileSize":            {build(t, stored("zeros.bz2", bzip2Zeros)), limits, zipcheck.ErrTooLarge},
		"a ZIP in a gzip past MaxFileSize":    {build(t, stored("inner.zip.gz", gzOf(t, zipOf(deflated("zeros.bin", zeros(2<<20)))))), limits, zipcheck.ErrTooLarge},
		"a tar entry past MaxFileSize":        {build(t, deflated("data.tar", tarOf(t, false, stored("zeros.bin", zeros(2<<20))))), limits, zipcheck.ErrTooLarge},
		"a ZIP in a tar.gz past MaxFileSize":  {build(t, stored("data.tgz", gzOf(t, tarOf(t, false, stored("inner.zip", zipOf(deflated("zeros.bin", zeros(2<<20)))))))), limits, zipcheck.ErrTooLarge},
		"tar entries past MaxFiles":           {build(t, deflated("data.tar", tarOf(t, false, stored("a", csv), stored("b", csv), stored("c", csv)))), fewFiles, zipcheck.ErrTooLarge},
		"a gzip with a bad checksum":          {build(t, stored("data.gz", badCRC)), limits, zipcheck.ErrInvalid},
		"a corrupt bzip2":                     {build(t, stored("data.bz2", badBzip2)), limits, zipcheck.ErrInvalid},
		"a gzip cut short":                    {build(t, stored("data.gz", gzOf(t, text(5000))[:2000])), limits, zipcheck.ErrInvalid},
		"a gzip of two members":               {build(t, stored("data.gz", append(gzOf(t, csv), gzOf(t, csv)...))), limits, zipcheck.ErrNested},
		"a tar in a tar in a tar in a tar.gz": {build(t, stored("x.tgz", gzOf(t, tarOf(t, false, stored("y.tar", tarOf(t, false, stored("z.tar", tarOf(t, false, stored("a", csv))))))))), limits, zipcheck.ErrNested},
	} {
		t.Run(name, func(t *testing.T) {
			refused(t, c.data, c.limits, c.want)
		})
	}
}

// An archive of a format clamd opens but core does not (7-Zip, RAR, xz,
// cab, cpio, ARJ, LHA, ISO 9660, XAR, EGG, ALZip), told by its first bytes,
// is refused as nested: core cannot see what clamd would leave unread inside
// it.
func TestCheckRefusesArchivesCoreCannotOpen(t *testing.T) {
	t.Parallel()
	iso := text(40000)
	copy(iso[32769:], "CD001")
	for name, data := range map[string][]byte{
		"7-Zip": append([]byte("7z\xbc\xaf\x27\x1c"), text(100)...),
		"RAR":   append([]byte("Rar!\x1a\x07\x01\x00"), text(100)...),
		"xz":    append([]byte("\xfd7zXZ\x00"), text(100)...),
		"cab":   append([]byte("MSCF\x00\x00\x00\x00"), text(100)...),
		"cpio":  append([]byte("070701"), text(100)...),
		"ARJ":   append(arjHeader(), text(100)...),
		"LHA":   append([]byte{0x20, 0x00, '-', 'l', 'h', '5', '-'}, text(100)...),
		"ISO":   iso,
		"XAR":   append([]byte("xar!\x00\x1c"), text(100)...),
		"EGG":   append([]byte("EGGA"), text(100)...),
		"ALZip": append([]byte("ALZ\x01"), text(100)...),
	} {
		t.Run(name, func(t *testing.T) {
			refused(t, build(t, deflated("data.bin", data)), limits, zipcheck.ErrNested)
		})
	}
}

// What merely starts like an archive is none: a static library (ar, which
// clamd does not open), a text that starts with "BZh" or looks like an LHA
// header, bytes 1f 8b that are no deflate gzip, a cpio-like number, an ARJ
// mark whose header does not check out, and an RPM package's lead.
func TestCheckTakesNoLookalikeForAnArchive(t *testing.T) {
	t.Parallel()
	passes(t, build(t,
		stored("lib/libsky.a", append([]byte("!<arch>\n"), text(200)...)),
		deflated("notlar/bzh.txt", []byte("BZhello, SKY LAB")),
		deflated("scripts/build.sh", []byte("# -lib-x build\n")),
		stored("veri/binary.dat", append([]byte{0x1f, 0x8b, 0x00}, text(100)...)),
		deflated("veri/sayı.txt", []byte("070709 not a cpio archive")),
		stored("arj-like.bin", append([]byte{0x60, 0xea, 30, 0}, text(100)...)),
		stored("rpm-like.bin", append([]byte{0xed, 0xab, 0xee, 0xdb}, text(100)...)),
	), limits)
}
