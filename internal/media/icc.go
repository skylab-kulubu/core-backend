package media

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"hash/crc32"
	"io"
	"log"
	"slices"
	"sync"
)

// maxICCBytes is the largest colour profile core carries over.
const maxICCBytes = 1 << 20

// Colour profiles (D3): core does no colour conversion. It carries an
// image's colours over into the re-encoded image and every size by
// embedding a colour profile rebuilt from the uploaded one (rebuildICC),
// never the uploaded bytes themselves. An image whose profile cannot be
// rebuilt gets none, and is then shown as sRGB.

// imageICC is the colour profile an image carries (JPEG APP2 ICC_PROFILE,
// PNG iCCP, WebP ICCP), rebuilt; nil when it has none or it cannot be
// rebuilt.
func imageICC(data []byte) []byte {
	switch {
	case isJPEG(data):
		return rebuildICC(jpegICCProfile(data))
	case isPNG(data):
		return rebuildICC(pngICCProfile(data))
	case isWebP(data):
		return rebuildICC(webpICCProfile(data))
	}
	return nil
}

// iccKeptTags are the tags a rebuilt profile keeps, in order: its
// description and copyright, and the white point, adaptation, primaries
// and transfer curves that define a matrix/TRC RGB profile's colours.
var iccKeptTags = []string{"desc", "cprt", "wtpt", "chad", "rXYZ", "gXYZ", "bXYZ", "rTRC", "gTRC", "bTRC"}

// iccRequiredTags are the tags without which a profile is not a matrix/TRC
// RGB profile (a profile of lookup tables only is dropped).
var iccRequiredTags = []string{"wtpt", "rXYZ", "gXYZ", "bXYZ", "rTRC", "gTRC", "bTRC"}

// maxICCText is the longest description or copyright element kept.
const maxICCText = 4096

// rebuildICC builds a fresh profile from an uploaded one: only a monitor
// or scanner RGB profile (device class mntr or scnr, colour space RGB, PCS
// XYZ or Lab) with its matrix and curves, keeping the tags in iccKeptTags
// once each is checked against its type (XYZ, sf32, curv or para, mluc,
// desc or text) with every length in bounds, under a new header (its size
// recomputed, its profile ID and everything identifying the maker zeroed)
// and a new tag table. Anything else, a profile above maxICCBytes, one
// without the 'acsp' signature or its own size, or a tag of any kind that
// leaves the profile, is nil: no profile. A profile whose rebuilding panics
// is nil too (guardICC).
func rebuildICC(profile []byte) []byte {
	return guardICC(rebuildICCTags, profile)
}

// iccPanicLogged logs the first profile dropped because rebuilding it
// panicked, once per process.
var iccPanicLogged sync.Once

// guardICC runs build on a profile. A build that panics drops the profile
// (nil: the image is shown as sRGB) instead of refusing the upload or
// stopping core; the first such panic is logged.
func guardICC(build func([]byte) []byte, profile []byte) (rebuilt []byte) {
	defer func() {
		if recovered := recover(); recovered != nil {
			rebuilt = nil
			iccPanicLogged.Do(func() {
				log.Printf("media: dropped a colour profile: rebuilding it panicked: %v", recovered)
			})
		}
	}()
	return build(profile)
}

// rebuildICCTags is rebuildICC without its guard.
func rebuildICCTags(profile []byte) []byte {
	be := binary.BigEndian
	if len(profile) < 132 || len(profile) > maxICCBytes || int(be.Uint32(profile)) != len(profile) || string(profile[36:40]) != "acsp" {
		return nil
	}
	class, space, pcs := string(profile[12:16]), string(profile[16:20]), string(profile[20:24])
	if (class != "mntr" && class != "scnr") || space != "RGB " || (pcs != "XYZ " && pcs != "Lab ") {
		return nil
	}
	count := int(be.Uint32(profile[128:]))
	table := 132 + 12*count
	if count == 0 || count > 256 || table > len(profile) {
		return nil
	}
	tags := map[string][]byte{}
	for i := 0; i < count; i++ {
		entry := profile[132+12*i:]
		sig := string(entry[:4])
		offset, size := int(be.Uint32(entry[4:])), int(be.Uint32(entry[8:]))
		if offset < table || offset+size > len(profile) {
			return nil
		}
		if _, seen := tags[sig]; seen || !slices.Contains(iccKeptTags, sig) {
			continue
		}
		if element := iccElement(sig, profile[offset:offset+size]); element != nil {
			tags[sig] = element
		}
	}
	for _, sig := range iccRequiredTags {
		if tags[sig] == nil {
			return nil
		}
	}
	var kept []string
	for _, sig := range iccKeptTags {
		if tags[sig] != nil {
			kept = append(kept, sig)
		}
	}
	out := make([]byte, 132+12*len(kept))
	copy(out[8:12], profile[8:12])   // version
	copy(out[12:24], profile[12:24]) // class, colour space, PCS
	copy(out[36:40], "acsp")
	copy(out[64:80], profile[64:80]) // rendering intent, illuminant
	be.PutUint32(out[128:], uint32(len(kept)))
	for i, sig := range kept {
		for len(out)%4 != 0 {
			out = append(out, 0)
		}
		entry := out[132+12*i:]
		copy(entry, sig)
		be.PutUint32(entry[4:], uint32(len(out)))
		be.PutUint32(entry[8:], uint32(len(tags[sig])))
		out = append(out, tags[sig]...)
	}
	be.PutUint32(out, uint32(len(out)))
	return out
}

// iccElement is a tag's element checked against the types the tag may
// have, every length it declares in bounds, trimmed to its own length (a
// v2 description rebuilt), or nil when it does not check.
func iccElement(sig string, element []byte) []byte {
	be := binary.BigEndian
	if len(element) < 12 {
		return nil
	}
	kind := string(element[:4])
	switch sig {
	case "wtpt", "rXYZ", "gXYZ", "bXYZ":
		// 'XYZ ', reserved, one XYZNumber.
		if kind == "XYZ " && len(element) >= 20 {
			return element[:20]
		}
	case "chad":
		// 'sf32', reserved, a 3x3 matrix.
		if kind == "sf32" && len(element) >= 44 {
			return element[:44]
		}
	case "rTRC", "gTRC", "bTRC":
		switch kind {
		case "curv":
			// 'curv', reserved, entry count, the entries.
			entries := int(be.Uint32(element[8:]))
			if entries <= 65536 && len(element) >= 12+2*entries {
				return element[:12+2*entries]
			}
		case "para":
			// 'para', reserved, function type, reserved, its parameters.
			params := map[uint16]int{0: 1, 1: 3, 2: 4, 3: 5, 4: 7}
			n, ok := params[be.Uint16(element[8:])]
			if ok && len(element) >= 12+4*n {
				return element[:12+4*n]
			}
		}
	case "desc", "cprt":
		if len(element) > maxICCText {
			return nil
		}
		switch kind {
		case "mluc":
			return iccMultiLocalized(element)
		case "desc":
			return iccTextDescription(element)
		case "text":
			// 'text', reserved, the text.
			return element
		}
	}
	return nil
}

// iccMultiLocalized is an mluc element whose header, records and strings
// are all in bounds: 'mluc', reserved, record count, record size (12),
// then per record a language, a country, its string's length and offset.
func iccMultiLocalized(element []byte) []byte {
	be := binary.BigEndian
	if len(element) < 16 {
		return nil
	}
	records, recordSize := int(be.Uint32(element[8:])), int(be.Uint32(element[12:]))
	if records < 1 || records > 32 || recordSize != 12 {
		return nil
	}
	strings := 16 + 12*records
	if strings > len(element) {
		return nil
	}
	for r := 0; r < records; r++ {
		record := element[16+12*r : 16+12*(r+1)]
		length, offset := int(be.Uint32(record[4:])), int(be.Uint32(record[8:]))
		if length%2 != 0 || offset < strings || offset+length > len(element) {
			return nil
		}
	}
	return element
}

// iccTextDescription rebuilds a v2 textDescription element around its
// ASCII description: 'desc', reserved, the ASCII count and text (NUL
// ended), then an empty Unicode and ScriptCode description, so a reader
// that follows every count stays inside it.
func iccTextDescription(element []byte) []byte {
	be := binary.BigEndian
	ascii := int(be.Uint32(element[8:]))
	if ascii < 1 || 12+ascii > len(element) {
		return nil
	}
	text := element[12 : 12+ascii]
	if end := bytes.IndexByte(text, 0); end >= 0 {
		text = text[:end]
	}
	out := append([]byte("desc\x00\x00\x00\x00"), be.AppendUint32(nil, uint32(len(text)+1))...)
	out = append(append(out, text...), 0)
	// Unicode language and count, ScriptCode code and count, and the
	// 67-byte ScriptCode field.
	return append(out, make([]byte, 4+4+2+1+67)...)
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
		if pos+12+size > len(data) {
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
	chunks, err := webpChunks(data)
	if err != nil {
		return nil
	}
	for _, chunk := range chunks {
		if chunk.fourcc == "ICCP" {
			return chunk.payload
		}
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
