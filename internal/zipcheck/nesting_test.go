package zipcheck_test

import (
	"archive/zip"
	"bytes"
	"compress/gzip"
	"testing"

	"github.com/skylab-kulubu/core-backend/internal/zipcheck"
)

// An archive inside the ZIP is accepted one level deep, and only when it is
// a ZIP core reads the same way: clamd scans what is inside it too, and
// skips there what it skips at the top (checked: a nested ZIP's member past
// MaxFileSize went unscanned). Its members count toward the same limits.
// An archive inside that one, an archive of another format (by its name or
// its first bytes), and any nesting while clamd's MaxRecursion would not
// reach the members of a nested ZIP are refused as too large.
func TestCheckNestedArchives(t *testing.T) {
	t.Parallel()
	inner := build(t, deflated("a.txt", bytes.Repeat([]byte("a"), 4000)), stored("b.bin", text(300)))
	gzipped := append([]byte{0x1f, 0x8b, 8, 0}, text(200)...)
	tarball := make([]byte, 1024)
	copy(tarball, "data.csv")
	copy(tarball[257:], "ustar\x0000")

	passes(t, build(t, deflated("inner.zip", inner), stored("notes.txt", []byte("notes"))), limits)
	passes(t, build(t, stored("inner.zip", inner)), limits)
	passes(t, build(t, deflated("rapor.docx", inner), deflated("slides.PPTX", inner)), limits)
	passes(t, build(t, stored("empty.zip", nil), deflated("data.csv", []byte("a,b\n1,2\n"))), limits)

	lyingInner := build(t, file{name: "zeros.bin", data: zeros(300 << 10), method: zip.Deflate, sized: true})
	l := layoutOf(t, lyingInner)
	put32(lyingInner, l.dir[0]+dirUSize, 100)
	put32(lyingInner, l.local[0]+locUSize, 100)
	cutInner := append(append([]byte{}, inner...), "tail"...)

	for name, c := range map[string]struct {
		data   []byte
		limits zipcheck.Limits
		want   error
	}{
		"a ZIP in a ZIP in the ZIP": {build(t, deflated("inner.zip", build(t, deflated("deeper.zip", inner)))), limits, zipcheck.ErrTooLarge},
		"a ZIP named as another in a ZIP in the ZIP": {
			build(t, deflated("inner.zip", build(t, deflated("deeper.bin", inner)))), limits, zipcheck.ErrTooLarge,
		},
		"a nested member past MaxFileSize": {build(t, deflated("inner.zip", build(t, deflated("zeros.bin", zeros(2<<20))))), limits, zipcheck.ErrTooLarge},
		"a nested member that lies":        {build(t, deflated("inner.zip", lyingInner)), limits, zipcheck.ErrInvalid},
		"a nested ZIP with bytes after its end": {
			build(t, deflated("inner.zip", cutInner)), limits, zipcheck.ErrInvalid,
		},
		"more members than MaxFiles in all": {
			build(t, deflated("inner.zip", inner), stored("c.txt", nil)),
			zipcheck.Limits{MaxFileSize: 1 << 20, MaxScanSize: 4 << 20, MaxFiles: 3, MaxRecursion: 17}, zipcheck.ErrTooLarge,
		},
		"a gzip by its first bytes": {build(t, deflated("data.bin", gzipped)), limits, zipcheck.ErrTooLarge},
		"a tar by its first bytes":  {build(t, deflated("data", tarball)), limits, zipcheck.ErrTooLarge},
		"a 7z by its name":          {build(t, stored("backup.7z", text(100))), limits, zipcheck.ErrTooLarge},
		"a RAR by its name":         {build(t, stored("backup.RAR", text(100))), limits, zipcheck.ErrTooLarge},
		"a ZIP by its name only":    {build(t, stored("not-really.zip", text(100))), limits, zipcheck.ErrTooLarge},
		"a tar.gz by its name":      {build(t, stored("veri.tar.gz", text(100))), limits, zipcheck.ErrTooLarge},
		"a ZIP where MaxRecursion is 2": {
			build(t, deflated("inner.zip", inner)),
			zipcheck.Limits{MaxFileSize: 1 << 20, MaxScanSize: 4 << 20, MaxFiles: 100, MaxRecursion: 2}, zipcheck.ErrTooLarge,
		},
	} {
		t.Run(name, func(t *testing.T) {
			refused(t, c.data, c.limits, c.want)
		})
	}
}

// A nested ZIP larger than what the check keeps of a member's end (8 MiB)
// is read from its last bytes all the same.
func TestCheckReadsTheDirectoryOfALargeNestedZIPFromItsEnd(t *testing.T) {
	t.Parallel()
	inner := build(t, stored("a.bin", text(5<<20)), deflated("b.txt", bytes.Repeat([]byte("b"), 4<<20)), stored("c.bin", text(1<<20)))
	roomy := zipcheck.Limits{MaxFileSize: 16 << 20, MaxScanSize: 64 << 20, MaxFiles: 100, MaxRecursion: 17}
	passes(t, build(t, deflated("inner.zip", inner)), roomy)
	passes(t, build(t, stored("inner.zip", inner)), roomy)
}

// A member is an archive by its name or its first bytes, and only formats
// clamd opens count. What merely starts like one is no archive: a static
// library (ar, which clamd does not open), a text that starts with "BZh" or
// looks like an LHA header, bytes 1f 8b that are no deflate gzip, a cpio-like
// number, and Debian or RPM packages by name.
func TestCheckTakesNoLookalikeForAnArchive(t *testing.T) {
	t.Parallel()
	passes(t, build(t,
		stored("lib/libsky.a", append([]byte("!<arch>\n"), text(200)...)),
		deflated("notlar/bzh.txt", []byte("BZhello, SKY LAB")),
		deflated("scripts/build.sh", []byte("# -lib-x build\n")),
		stored("veri/binary.dat", append([]byte{0x1f, 0x8b, 0x00}, text(100)...)),
		deflated("veri/sayı.txt", []byte("070709 not a cpio archive")),
		stored("paket/sky.deb", text(300)),
		stored("paket/sky.rpm", text(300)),
		stored("rpm-like.bin", append([]byte{0xed, 0xab, 0xee, 0xdb}, text(100)...)),
	), limits)
}

// Real archives of the formats clamd opens are still caught by their first
// bytes, whatever their names.
func TestCheckCatchesRealNestedArchivesByTheirFirstBytes(t *testing.T) {
	t.Parallel()
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	if _, err := zw.Write([]byte("hidden")); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	lha := append([]byte{0x20, 0x00}, "-lh5-"...)
	for name, data := range map[string][]byte{
		"gzip":        gz.Bytes(),
		"bzip2":       append([]byte("BZh91AY&SY"), text(100)...),
		"empty bzip2": []byte("BZh9\x17\x72\x45\x38\x50\x90\x00\x00\x00\x00"),
		"cpio":        append([]byte("070701"), text(100)...),
		"LHA":         append(lha, text(100)...),
		"7-Zip":       append([]byte("7z\xbc\xaf\x27\x1c"), text(100)...),
		"RAR":         append([]byte("Rar!\x1a\x07\x01\x00"), text(100)...),
		"xz":          append([]byte("\xfd7zXZ\x00"), text(100)...),
		"cab":         append([]byte("MSCF\x00\x00\x00\x00"), text(100)...),
	} {
		t.Run(name, func(t *testing.T) {
			refused(t, build(t, deflated("data.bin", data)), limits, zipcheck.ErrTooLarge)
		})
	}
}
