package media

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"hash/crc32"
	"io"
)

// maxICCBytes is the largest colour profile core carries over.
const maxICCBytes = 1 << 20

// Colour profiles (D3): core does no colour conversion. It carries an
// image's ICC profile over into the re-encoded image and every size, as
// the image had it, so a wide-gamut photo (Display P3) keeps its colours.

// imageICC is the ICC profile an image carries (JPEG APP2 ICC_PROFILE, PNG
// iCCP, WebP ICCP), or nil when it has none or it is not a valid one
// (validICC).
func imageICC(data []byte) []byte {
	var profile []byte
	switch {
	case isJPEG(data):
		profile = jpegICCProfile(data)
	case isPNG(data):
		profile = pngICCProfile(data)
	case isWebP(data):
		profile = webpICCProfile(data)
	}
	if !validICC(profile) {
		return nil
	}
	return profile
}

// validICC reports whether a profile is one: within maxICCBytes, its header
// giving its own size, and the 'acsp' signature at byte 36.
func validICC(profile []byte) bool {
	return len(profile) >= 132 && len(profile) <= maxICCBytes &&
		int(binary.BigEndian.Uint32(profile)) == len(profile) && string(profile[36:40]) == "acsp"
}

// jpegICCProfile reassembles a profile from the APP2 ICC_PROFILE segments
// before the image data, when every one of them is there.
func jpegICCProfile(data []byte) []byte {
	var chunks [][]byte
	count := 0
	for pos := 2; pos+4 <= len(data) && data[pos] == 0xFF; {
		marker := data[pos+1]
		if marker == 0xDA || marker == 0xD9 {
			break
		}
		size := int(binary.BigEndian.Uint16(data[pos+2:]))
		if size < 2 || pos+2+size > len(data) {
			break
		}
		payload := data[pos+4 : pos+2+size]
		if marker == 0xE2 && len(payload) >= 14 && bytes.HasPrefix(payload, []byte("ICC_PROFILE\x00")) {
			seq, total := int(payload[12]), int(payload[13])
			if count == 0 {
				count = total
				chunks = make([][]byte, total)
			}
			if total != count || seq < 1 || seq > count || chunks[seq-1] != nil {
				return nil
			}
			chunks[seq-1] = payload[14:]
		}
		pos += 2 + size
	}
	var profile []byte
	for _, chunk := range chunks {
		if chunk == nil {
			return nil
		}
		profile = append(profile, chunk...)
		if len(profile) > maxICCBytes {
			return nil
		}
	}
	return profile
}

// pngICCProfile inflates a PNG's iCCP chunk, at most maxICCBytes.
func pngICCProfile(data []byte) []byte {
	for pos := 8; pos+12 <= len(data); {
		size := int(binary.BigEndian.Uint32(data[pos:]))
		if size < 0 || pos+12+size > len(data) {
			return nil
		}
		kind := string(data[pos+4 : pos+8])
		if kind == "IDAT" {
			return nil
		}
		if kind == "iCCP" {
			payload := data[pos+8 : pos+8+size]
			name := bytes.IndexByte(payload, 0)
			if name < 1 || name > 79 || name+2 > len(payload) || payload[name+1] != 0 {
				return nil
			}
			r, err := zlib.NewReader(bytes.NewReader(payload[name+2:]))
			if err != nil {
				return nil
			}
			profile, err := io.ReadAll(io.LimitReader(r, maxICCBytes+1))
			if err != nil {
				return nil
			}
			return profile
		}
		pos += 12 + size
	}
	return nil
}

// webpICCProfile is an extended WebP's ICCP chunk.
func webpICCProfile(data []byte) []byte {
	for pos := 12; pos+8 <= len(data); {
		size := int(binary.LittleEndian.Uint32(data[pos+4:]))
		if size < 0 || pos+8+size > len(data) {
			return nil
		}
		if string(data[pos:pos+4]) == "ICCP" {
			return data[pos+8 : pos+8+size]
		}
		pos += 8 + size + size&1
	}
	return nil
}

// jpegICCChunk is the most profile bytes an APP2 segment holds: 65535, less
// its length, the ICC_PROFILE name and the sequence bytes.
const jpegICCChunk = 65535 - 2 - 14

// withICC embeds a profile into an image core encoded: after a JPEG's SOI
// as APP2 ICC_PROFILE segments, or after a PNG's IHDR as an iCCP chunk. An
// image of another type, or no profile, is returned as it is.
func withICC(body []byte, ctype string, profile []byte) []byte {
	if len(profile) == 0 {
		return body
	}
	switch ctype {
	case "image/jpeg":
		count := (len(profile) + jpegICCChunk - 1) / jpegICCChunk
		out := append(make([]byte, 0, len(body)+len(profile)+count*18), body[:2]...)
		for i := 0; i < count; i++ {
			chunk := profile[i*jpegICCChunk : min(len(profile), (i+1)*jpegICCChunk)]
			out = append(out, 0xFF, 0xE2)
			out = binary.BigEndian.AppendUint16(out, uint16(2+14+len(chunk)))
			out = append(out, "ICC_PROFILE\x00"...)
			out = append(out, byte(i+1), byte(count))
			out = append(out, chunk...)
		}
		return append(out, body[2:]...)
	case "image/png":
		var compressed bytes.Buffer
		w := zlib.NewWriter(&compressed)
		_, _ = w.Write(profile)
		_ = w.Close()
		payload := append([]byte("ICC Profile\x00\x00"), compressed.Bytes()...)
		chunk := binary.BigEndian.AppendUint32(nil, uint32(len(payload)))
		chunk = append(chunk, "iCCP"...)
		chunk = append(chunk, payload...)
		chunk = binary.BigEndian.AppendUint32(chunk, crc32.ChecksumIEEE(chunk[4:]))
		// Signature (8) and IHDR (25), then the profile, which must come
		// before the image data.
		out := append(make([]byte, 0, len(body)+len(chunk)), body[:33]...)
		out = append(out, chunk...)
		return append(out, body[33:]...)
	}
	return body
}
