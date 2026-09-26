package media

import (
	"bytes"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	KindImage = "IMAGE"
	KindFile  = "FILE"

	maxImageBytes = 10 * 1024 * 1024
	maxFileBytes  = 20 * 1024 * 1024

	// MaxUploadBytes is the largest file Upload accepts of any kind.
	MaxUploadBytes = maxFileBytes
)

var pngKeepAncillary = map[string]struct{}{
	"tRNS": {}, "gAMA": {}, "cHRM": {}, "sRGB": {}, "iCCP": {},
	"bKGD": {}, "pHYs": {}, "sBIT": {}, "hIST": {},
}

const (
	svgType = "image/svg+xml"
	pdfType = "application/pdf"
)

// rasterFormat is an image format Upload accepts as a raster image. The
// serving policy serves every one of them inline, so a format added here
// renders without touching the policy.
type rasterFormat struct {
	contentType string
	detect      func([]byte) bool
	strip       func([]byte) ([]byte, error)
}

var rasterFormats = []rasterFormat{
	{contentType: "image/jpeg", detect: isJPEG, strip: func(b []byte) ([]byte, error) { return stripJPEG(b), nil }},
	{contentType: "image/png", detect: isPNG, strip: func(b []byte) ([]byte, error) { return stripPNG(b), nil }},
	{contentType: "image/webp", detect: isWebP, strip: stripWebP},
	{contentType: "image/gif", detect: isGIF, strip: func(b []byte) ([]byte, error) { return stripGIF(b), nil }},
}

func isRasterType(contentType string) bool {
	for _, format := range rasterFormats {
		if format.contentType == contentType {
			return true
		}
	}
	return false
}

func isImage(data []byte) bool {
	for _, format := range rasterFormats {
		if format.detect(data) {
			return true
		}
	}
	return isSVG(data)
}

// detectContentType names the type of a file from its content: one of the
// raster formats, PDF when the file starts with its header, SVG when an
// <svg element opens in its first KiB, or "" for anything else. isPDF,
// which finds the header anywhere in the first KiB, stays the rule only for
// Media uploaded without a purpose. An SVG for a purpose that lists it is
// stored sanitized (sanitizeSVG).
//
// DOCX, ZIP and MP4 are detected when private Media and Direct upload
// arrive; until then nothing reaches a purpose that names them.
func detectContentType(data []byte) string {
	for _, format := range rasterFormats {
		if format.detect(data) {
			return format.contentType
		}
	}
	if bytes.HasPrefix(data, []byte("%PDF-")) {
		return pdfType
	}
	if isSVG(data) {
		return svgType
	}
	return ""
}

func sanitizeImage(data []byte) ([]byte, string, error) {
	if len(data) == 0 {
		return nil, "", ErrInvalid
	}
	for _, format := range rasterFormats {
		if format.detect(data) {
			out, err := format.strip(data)
			return out, format.contentType, err
		}
	}
	return nil, "", ErrInvalid
}

func isJPEG(b []byte) bool {
	return len(b) >= 3 && b[0] == 0xFF && b[1] == 0xD8 && b[2] == 0xFF
}

func isPNG(b []byte) bool {
	return len(b) >= 8 &&
		b[0] == 0x89 && b[1] == 'P' && b[2] == 'N' && b[3] == 'G' &&
		b[4] == 0x0D && b[5] == 0x0A && b[6] == 0x1A && b[7] == 0x0A
}

func isWebP(b []byte) bool {
	return len(b) >= 12 &&
		b[0] == 'R' && b[1] == 'I' && b[2] == 'F' && b[3] == 'F' &&
		b[8] == 'W' && b[9] == 'E' && b[10] == 'B' && b[11] == 'P'
}

func isGIF(b []byte) bool {
	return len(b) >= 6 &&
		b[0] == 'G' && b[1] == 'I' && b[2] == 'F' && b[3] == '8' &&
		(b[4] == '7' || b[4] == '9') && b[5] == 'a'
}

func isPDF(b []byte) bool {
	n := min(len(b), 1024)
	return bytes.Contains(b[:n], []byte("%PDF-"))
}

// stripJPEG removes a JPEG's metadata and everything after its primary
// image. Dropped: EXIF and XMP (APP1; the Orientation tag alone is kept, so
// a phone photo is not shown sideways), the MPF index of secondary images
// (APP2 "MPF"), Photoshop data (APP13) and comments. The file ends with the
// primary image's EOI: a phone's secondary images, each with its own EXIF
// and GPS, and a motion photo's video sit after it. Colour profiles (APP2
// ICC) and everything the decoder needs stay.
func stripJPEG(b []byte) []byte {
	if len(b) < 2 || b[0] != 0xFF || b[1] != 0xD8 {
		return b
	}
	out := []byte{0xFF, 0xD8}
	pos := 2
	keptOrientation := false
	for pos+2 <= len(b) {
		if b[pos] != 0xFF {
			break
		}
		marker := b[pos+1]
		switch {
		case marker == 0xFF:
			pos++ // fill byte
			continue
		case marker == 0xD9:
			return append(out, 0xFF, 0xD9)
		case marker == 0x01 || (marker >= 0xD0 && marker <= 0xD7):
			out = append(out, b[pos:pos+2]...)
			pos += 2
			continue
		}
		if pos+4 > len(b) {
			break
		}
		segLen := int(b[pos+2])<<8 | int(b[pos+3])
		segTotal := 2 + segLen
		if segLen < 2 || pos+segTotal > len(b) {
			break
		}
		payload := b[pos+4 : pos+segTotal]
		drop := marker == 0xE1 || marker == 0xED || marker == 0xFE ||
			(marker == 0xE2 && bytes.HasPrefix(payload, []byte("MPF\x00")))
		if !drop {
			out = append(out, b[pos:pos+segTotal]...)
		}
		if marker == 0xE1 && !keptOrientation {
			// EXIF goes, but its Orientation stays: without it a phone
			// photo shows sideways.
			if o, ok := exifOrientation(payload); ok && o != 1 {
				out = append(out, orientationSegment(o)...)
				keptOrientation = true
			}
		}
		pos += segTotal
		if marker == 0xDA {
			// The scan's entropy-coded data runs to the next marker: an 0xFF
			// not followed by a stuffed 0x00 or a restart marker.
			start := pos
			for pos+1 < len(b) && (b[pos] != 0xFF || b[pos+1] == 0x00 || (b[pos+1] >= 0xD0 && b[pos+1] <= 0xD7)) {
				pos++
			}
			out = append(out, b[start:pos]...)
		}
	}
	// A JPEG cut short keeps what it has of its primary image (a scan's
	// data is copied as it is read); bytes where a marker should be, and a
	// segment cut short, are not kept.
	return out
}

func stripPNG(b []byte) []byte {
	out := append([]byte{}, b[:8]...)
	pos := 8
	for pos+8 <= len(b) {
		length := readUInt32(b, pos)
		if length < 0 || pos+12+int(length) > len(b) {
			break
		}
		typ := string(b[pos+4 : pos+8])
		chunkTotal := 12 + int(length)
		r, _ := utf8.DecodeRuneInString(typ)
		critical := unicode.IsUpper(r)
		_, keepAncillary := pngKeepAncillary[typ]
		if critical || keepAncillary {
			out = append(out, b[pos:pos+chunkTotal]...)
		}
		pos += chunkTotal
		if typ == "IEND" {
			break
		}
	}
	return out
}

func readUInt32(b []byte, off int) int32 {
	return int32(uint32(b[off])<<24 | uint32(b[off+1])<<16 | uint32(b[off+2])<<8 | uint32(b[off+3]))
}

func stripWebP(b []byte) ([]byte, error) {
	if len(b) < 12 {
		return nil, ErrInvalid
	}
	var body bytes.Buffer
	pos := 12
	for pos+8 <= len(b) {
		fourcc := string(b[pos : pos+4])
		size := int(b[pos+4]) | int(b[pos+5])<<8 | int(b[pos+6])<<16 | int(b[pos+7])<<24
		chunkTotal := 8 + size + (size & 1)
		if pos+chunkTotal > len(b) {
			chunkTotal = len(b) - pos
		}
		drop := fourcc == "EXIF" || fourcc == "XMP "
		if !drop {
			if fourcc == "VP8X" && chunkTotal >= 9 {
				chunk := append([]byte{}, b[pos:pos+chunkTotal]...)
				chunk[8] = chunk[8] &^ 0x0C
				body.Write(chunk)
			} else {
				body.Write(b[pos : pos+chunkTotal])
			}
		}
		pos += chunkTotal
	}
	bodyBytes := body.Bytes()
	riffSize := 4 + len(bodyBytes)
	out := make([]byte, 0, 12+len(bodyBytes))
	out = append(out, 'R', 'I', 'F', 'F')
	out = append(out, byte(riffSize), byte(riffSize>>8), byte(riffSize>>16), byte(riffSize>>24))
	out = append(out, 'W', 'E', 'B', 'P')
	out = append(out, bodyBytes...)
	return out, nil
}

func stripGIF(b []byte) []byte {
	if len(b) < 13 {
		return nil
	}
	out := append([]byte{}, b[:13]...)
	pos := 13
	packed := int(b[10])
	if packed&0x80 != 0 {
		gctSize := 3 * (1 << ((packed & 0x07) + 1))
		if pos+gctSize > len(b) {
			return b
		}
		out = append(out, b[pos:pos+gctSize]...)
		pos += gctSize
	}
	for pos < len(b) {
		blockID := b[pos]
		switch blockID {
		case 0x3B:
			out = append(out, 0x3B)
			return out
		case 0x2C:
			if pos+10 > len(b) {
				return b
			}
			imgPacked := int(b[pos+9])
			p := pos + 10
			if imgPacked&0x80 != 0 {
				p += 3 * (1 << ((imgPacked & 0x07) + 1))
			}
			if p+1 > len(b) {
				return b
			}
			p++
			p = skipGIFSubBlocks(b, p)
			if p < 0 {
				return b
			}
			out = append(out, b[pos:p]...)
			pos = p
		case 0x21:
			if pos+2 > len(b) {
				return b
			}
			label := b[pos+1]
			dataStart := pos + 2
			end := skipGIFSubBlocks(b, dataStart)
			if end < 0 {
				return b
			}
			drop := label == 0xFE || (label == 0xFF && isGIFXMP(b, dataStart))
			if !drop {
				out = append(out, b[pos:end]...)
			}
			pos = end
		default:
			return b
		}
	}
	return out
}

func skipGIFSubBlocks(b []byte, p int) int {
	for p < len(b) {
		length := int(b[p])
		p++
		if length == 0 {
			return p
		}
		p += length
	}
	return -1
}

func isGIFXMP(b []byte, p int) bool {
	if p >= len(b) || b[p] != 11 || p+12 > len(b) {
		return false
	}
	return strings.HasPrefix(string(b[p+1:p+12]), "XMP Data")
}

func fileExtension(name string) (string, error) {
	i := strings.LastIndex(name, ".")
	if name == "" || i < 0 || i == len(name)-1 {
		return "", ErrInvalid
	}
	return strings.ToLower(name[i+1:]), nil
}
