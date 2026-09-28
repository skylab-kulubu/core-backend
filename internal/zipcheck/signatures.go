package zipcheck

import (
	"bytes"
	"hash/crc32"
	"strconv"
	"strings"
)

// headBytes is how much of a file's start tells what it is (ISO 9660's mark
// is at 32769).
const headBytes = 32774

// opener is how core reads a kind of file.
type opener int

const (
	// openPlain: no archive at its first byte; searched for archives past
	// it.
	openPlain opener = iota
	openZIP
	openGzip
	openBzip2
	openTar
	// openNone: an archive clamd opens and core does not.
	openNone
)

// kind is what a file is by its first bytes.
type kind struct {
	open opener
	name string
}

// IsZIP reports whether a file's first bytes are a ZIP's: a local header,
// the end record of an empty archive, or a spanned archive's mark. Office
// documents, JARs and APKs start so too.
func IsZIP(head []byte) bool {
	return bytes.HasPrefix(head, []byte("PK\x03\x04")) || bytes.HasPrefix(head, []byte("PK\x05\x06")) ||
		bytes.HasPrefix(head, []byte("PK\x07\x08"))
}

// kindOf tells a file by its first bytes: only a format's full signature
// counts, so that a file which merely starts like one is plain.
func kindOf(head []byte) kind {
	switch {
	case IsZIP(head):
		return kind{openZIP, "ZIP"}
	case len(head) >= 10 && head[0] == 0x1f && head[1] == 0x8b && head[2] == 8 && head[3]&0xe0 == 0:
		return kind{openGzip, "gzip"}
	case isBzip2(head):
		return kind{openBzip2, "bzip2"}
	case isTar(head):
		return kind{openTar, "tar"}
	case len(head) >= 7 && string(head[2:4]) == "-l" && (head[4] == 'h' || head[4] == 'z') && head[6] == '-':
		return kind{openNone, "LHA"}
	case len(head) >= headBytes && string(head[32769:32774]) == "CD001":
		return kind{openNone, "ISO 9660"}
	}
	for _, sig := range foreignArchives {
		if sig.matches(head, -1) {
			return kind{openNone, sig.name}
		}
	}
	return kind{}
}

// isBzip2 reports whether a file starts as a bzip2 stream: "BZh", a block
// size from 1 to 9, then a block's magic or the end of an empty stream.
func isBzip2(head []byte) bool {
	return len(head) >= 10 && string(head[:3]) == "BZh" && head[3] >= '1' && head[3] <= '9' &&
		(string(head[4:10]) == "1AY&SY" || string(head[4:10]) == "\x17\x72\x45\x38\x50\x90")
}

// isTar reports whether a file starts with a tar header whose checksum
// checks out, ustar or the oldest kind.
func isTar(head []byte) bool {
	if len(head) < 512 || head[0] == 0 {
		return false
	}
	field := strings.Trim(string(head[148:156]), " \x00")
	want, err := strconv.ParseUint(field, 8, 32)
	if field == "" || err != nil {
		return false
	}
	var sum uint64
	for i, b := range head[:512] {
		if i >= 148 && i < 156 {
			b = ' '
		}
		sum += uint64(b)
	}
	return sum == want
}

// signature is how an archive starts: its magic, and what must follow it.
type signature struct {
	name  string
	magic []byte
	// valid checks the bytes from the magic's first one on, left of the
	// file from there on (-1 when unknown); nil takes the magic alone.
	valid func(b []byte, left int64) bool
}

func (s signature) matches(b []byte, left int64) bool {
	return bytes.HasPrefix(b, s.magic) && (s.valid == nil || s.valid(b, left))
}

var (
	rarSignature = signature{"RAR", []byte("Rar!\x1a\x07"), func(b []byte, _ int64) bool {
		return len(b) >= 7 && (b[6] == 0 || (len(b) >= 8 && b[6] == 1 && b[7] == 0))
	}}
	sevenZipSignature = signature{"7-Zip", []byte("7z\xbc\xaf\x27\x1c"), nil}
	cabSignature      = signature{"cab", []byte("MSCF\x00\x00\x00\x00"), nil}
	arjSignature      = signature{"ARJ", []byte{0x60, 0xea}, validARJ}
)

// foreignArchives are the archives clamd opens and core does not, by their
// first bytes.
var foreignArchives = []signature{
	sevenZipSignature, rarSignature, cabSignature, arjSignature,
	{"xz", []byte("\xfd7zXZ\x00"), nil},
	{"cpio", []byte("070701"), nil},
	{"cpio", []byte("070702"), nil},
	{"cpio", []byte("070707"), nil},
	{"XAR", []byte("xar!"), nil},
	{"EGG", []byte("EGGA"), nil},
	{"ALZip", []byte("ALZ\x01"), nil},
}

// embeddedArchives are the archives clamd unpacks from past a file's first
// byte (checked for a ZIP), by signatures long or checked enough not to turn
// up by chance in other data: gzip's two bytes are left out.
var embeddedArchives = []signature{
	{"ZIP", localMark, unpackableLocalHeader},
	rarSignature, sevenZipSignature, cabSignature, arjSignature,
}

// unpackableLocalHeader reports whether b starts with a local header clamd
// would unpack on its own: a method the format defines, and a name, extra
// field and data that fit in the rest of the file (left bytes from the
// header on; -1 when unknown). clamd goes by nothing else: it unpacked lone
// headers of version 25.5, with no name or one of 2000 bytes, and with a
// reserved flag set (checked). Chance data passes it about once in 100,000
// GiB.
func unpackableLocalHeader(b []byte, left int64) bool {
	if len(b) < localLen || !zipMethods[le16(b[8:])] {
		return false
	}
	need := int64(localLen) + int64(le16(b[26:])) + int64(le16(b[28:])) + int64(le32(b[18:]))
	return left < 0 || need <= left
}

// zipMethods are the compression methods the format defines.
var zipMethods = map[uint16]bool{
	0: true, 1: true, 2: true, 3: true, 4: true, 5: true, 6: true, 8: true, 9: true, 10: true, 12: true, 14: true,
	16: true, 18: true, 19: true, 20: true, 93: true, 94: true, 95: true, 96: true, 97: true, 98: true, 99: true,
}

// validARJ reports whether b starts with an ARJ header that checks out: its
// size (at most 2600 bytes) and the CRC-32 that follows it.
func validARJ(b []byte, _ int64) bool {
	if len(b) < 4 {
		return false
	}
	size := int(le16(b[2:]))
	return size > 0 && size <= 2600 && len(b) >= 4+size+4 && crc32.ChecksumIEEE(b[4:4+size]) == le32(b[4+size:])
}

// scanLook is how many bytes past its first one a signature may need to
// be checked: an ARJ header and its CRC.
const scanLook = 4 + 2600 + 4

// scanner finds embedded archive signatures in a file fed to it in pieces,
// one that straddles two pieces included.
type scanner struct {
	// size is the file's size, -1 when unknown.
	size int64
	// buf holds the bytes not yet examined as a signature's start, and
	// what follows them.
	buf  []byte
	base int64
}

// feed takes the next piece of the file and calls hit for each signature
// that starts past the file's first byte. final examines what is left.
func (s *scanner) feed(p []byte, final bool, hit func(signature, int64) error) error {
	s.buf = append(s.buf, p...)
	end := len(s.buf)
	if !final {
		end -= scanLook
	}
	if end <= 0 {
		return nil
	}
	for _, sig := range embeddedArchives {
		for from := 0; from < end; {
			i := bytes.Index(s.buf[from:], sig.magic)
			if i < 0 || from+i >= end {
				break
			}
			i += from
			from = i + 1
			left := int64(-1)
			if s.size >= 0 {
				left = s.size - (s.base + int64(i))
			}
			if at := s.base + int64(i); at > 0 && sig.matches(s.buf[i:], left) {
				if err := hit(sig, at); err != nil {
					return err
				}
			}
		}
	}
	n := copy(s.buf, s.buf[end:])
	s.buf = s.buf[:n]
	s.base += int64(end)
	return nil
}

// from is what the scanner holds of the file from offset at on: the rest
// of what it was fed. It is only called from hit.
func (s *scanner) from(at int64) []byte { return s.buf[at-s.base:] }
