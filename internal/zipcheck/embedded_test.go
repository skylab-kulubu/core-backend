package zipcheck_test

import (
	"testing"

	"github.com/skylab-kulubu/core-backend/internal/zipcheck"
)

// clamd unpacks a ZIP it finds past the first byte of a file (a
// self-extracting program, an archive appended to an image), and skips
// there what it skips anywhere (checked: the ZIP after a stub is unpacked,
// and its member past MaxFileSize goes unscanned). So a member holding a ZIP
// past its first byte has that ZIP checked as a nested one: from the member
// in the file when the member is stored, from memory otherwise. Its offsets
// may count from the ZIP or, as zip -A leaves a self-extractor, from the
// member's first byte. One that cannot be checked (it does not end the
// member) is refused as nested.
func TestCheckChecksAZIPEmbeddedInAMember(t *testing.T) {
	t.Parallel()
	csv := []byte("ad,soyad\nAda,Lovelace\n")
	small := build(t, sized(deflated("a.csv", csv), stored("b.bin", text(300)))...)
	big := build(t, sized(deflated("zeros.bin", zeros(2<<20)))...)
	large := build(t, sized(stored("a.bin", text(20<<10)))...)
	exe := stub(4096)
	for name, data := range map[string][]byte{
		"after a stub, stored":                    build(t, stored("setup.exe", sfx(t, exe, small, false)), filler()),
		"after a stub, deflated":                  build(t, deflated("setup.exe", sfx(t, exe, small, false))),
		"with offsets from the stub, stored":      build(t, stored("setup.exe", sfx(t, exe, small, true)), filler()),
		"with offsets from the stub, deflated":    build(t, deflated("setup.exe", sfx(t, exe, small, true))),
		"across the 32 KiB the scan reads a time": build(t, deflated("setup.exe", sfx(t, stub(32<<10-2), small, false))),
		"in a tar entry":                          build(t, deflated("data.tar", tarOf(t, false, stored("setup.exe", sfx(t, exe, small, false))))),
	} {
		t.Run(name, func(t *testing.T) {
			passes(t, data, limits)
		})
	}

	fits := limits
	fits.MaxBuffer = 16 << 10
	for name, c := range map[string]struct {
		data   []byte
		limits zipcheck.Limits
		want   error
	}{
		"a member past MaxFileSize, stored":            {build(t, stored("setup.exe", sfx(t, exe, big, false)), filler()), limits, zipcheck.ErrTooLarge},
		"a member past MaxFileSize, deflated":          {build(t, deflated("setup.exe", sfx(t, exe, big, false))), limits, zipcheck.ErrTooLarge},
		"offsets from the stub, past MaxFileSize":      {build(t, deflated("setup.exe", sfx(t, exe, big, true))), limits, zipcheck.ErrTooLarge},
		"in a tar entry, past MaxFileSize":             {build(t, deflated("data.tar", tarOf(t, false, stored("setup.exe", sfx(t, exe, big, false))))), limits, zipcheck.ErrTooLarge},
		"one that does not end the member":             {build(t, deflated("setup.exe", append(sfx(t, exe, small, false), stub(100)...))), limits, zipcheck.ErrNested},
		"one larger than MaxBuffer, deflated":          {build(t, deflated("setup.exe", sfx(t, exe, large, false)), filler()), fits, zipcheck.ErrNested},
		"one in a stored member larger than MaxBuffer": {build(t, stored("setup.exe", sfx(t, stub(20<<10), big, false)), filler()), fits, zipcheck.ErrTooLarge},
	} {
		t.Run(name, func(t *testing.T) {
			refused(t, c.data, c.limits, c.want)
		})
	}
}

// An archive of another format clamd unpacks from past a file's first byte
// (RAR, 7-Zip, cab, ARJ) is refused as nested, whatever holds it. Only
// full signatures count: an ARJ mark whose header does not check out, or a
// ZIP local header no ZIP writer would write, is no archive.
func TestCheckRefusesOtherArchivesEmbeddedInAMember(t *testing.T) {
	t.Parallel()
	exe := stub(1000)
	for name, embedded := range map[string][]byte{
		"RAR":   append([]byte("Rar!\x1a\x07\x00"), text(100)...),
		"7-Zip": append([]byte("7z\xbc\xaf\x27\x1c"), text(100)...),
		"cab":   append([]byte("MSCF\x00\x00\x00\x00"), text(100)...),
		"ARJ":   append(arjHeader(), text(100)...),
	} {
		t.Run(name, func(t *testing.T) {
			refused(t, build(t, deflated("setup.exe", append(append([]byte{}, exe...), embedded...))), limits, zipcheck.ErrNested)
			refused(t, build(t, stored("data.gz", gzOf(t, append(append([]byte{}, exe...), embedded...)))), limits, zipcheck.ErrNested)
		})
	}
	notZIP := append([]byte("PK\x03\x04"), 0xff, 0xff, 0xff, 0xff)
	passes(t, build(t,
		deflated("a.bin", append(append([]byte{}, exe...), notZIP...)),
		deflated("b.bin", append(append([]byte{}, exe...), 0x60, 0xea, 30, 0)),
		deflated("c.bin", append(append([]byte{}, exe...), "MSCF\x01\x00\x00\x00"...)),
	), limits)
}
