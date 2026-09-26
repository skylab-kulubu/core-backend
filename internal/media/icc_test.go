package media_test

import (
	"bytes"
	"compress/zlib"
	"context"
	"encoding/binary"
	"hash/crc32"
	"image/color"
	"io"
	"sort"
	"testing"
)

// iccTag is a tag of an ICC profile: its signature and its element.
type iccTag struct {
	sig  string
	data []byte
}

// buildICC is an ICC profile of the device class, colour space and PCS
// with the tags, laid out as a profiler writes one (profile ID set).
func buildICC(class, space, pcs string, tags []iccTag) []byte {
	table := 132 + 12*len(tags)
	out := make([]byte, table)
	copy(out[4:], "appl")
	binary.BigEndian.PutUint32(out[8:], 0x04400000)
	copy(out[12:], class)
	copy(out[16:], space)
	copy(out[20:], pcs)
	copy(out[36:], "acsp")
	copy(out[40:], "APPL")
	binary.BigEndian.PutUint32(out[68:], 0x0000F6D6)
	binary.BigEndian.PutUint32(out[72:], 0x00010000)
	binary.BigEndian.PutUint32(out[76:], 0x0000D32D)
	copy(out[80:], "appl")
	copy(out[84:], "0123456789abcdef") // profile ID
	binary.BigEndian.PutUint32(out[128:], uint32(len(tags)))
	for i, tag := range tags {
		for len(out)%4 != 0 {
			out = append(out, 0)
		}
		entry := 132 + 12*i
		copy(out[entry:], tag.sig)
		binary.BigEndian.PutUint32(out[entry+4:], uint32(len(out)))
		binary.BigEndian.PutUint32(out[entry+8:], uint32(len(tag.data)))
		out = append(out, tag.data...)
	}
	binary.BigEndian.PutUint32(out[0:], uint32(len(out)))
	return out
}

func s15(v float64) []byte { return binary.BigEndian.AppendUint32(nil, uint32(int32(v*65536))) }

func xyzTag(x, y, z float64) []byte {
	out := append([]byte("XYZ \x00\x00\x00\x00"), s15(x)...)
	return append(append(out, s15(y)...), s15(z)...)
}

// paraTag is the sRGB transfer curve as a parametric curve (type 3).
func paraTag() []byte {
	out := []byte("para\x00\x00\x00\x00\x00\x03\x00\x00")
	for _, v := range []float64{2.4, 1 / 1.055, 0.055 / 1.055, 1 / 12.92, 0.04045} {
		out = append(out, s15(v)...)
	}
	return out
}

// curvTag is a sampled transfer curve of n entries.
func curvTag(n int) []byte {
	out := binary.BigEndian.AppendUint32([]byte("curv\x00\x00\x00\x00"), uint32(n))
	for i := 0; i < n; i++ {
		out = binary.BigEndian.AppendUint16(out, uint16(i*65535/max(1, n-1)))
	}
	return out
}

func mlucTag(text string) []byte {
	out := []byte("mluc\x00\x00\x00\x00")
	out = binary.BigEndian.AppendUint32(out, 1)
	out = binary.BigEndian.AppendUint32(out, 12)
	out = append(out, "enUS"...)
	out = binary.BigEndian.AppendUint32(out, uint32(2*len(text)))
	out = binary.BigEndian.AppendUint32(out, 28)
	for _, r := range text {
		out = binary.BigEndian.AppendUint16(out, uint16(r))
	}
	return out
}

func chadTag() []byte {
	out := []byte("sf32\x00\x00\x00\x00")
	for _, v := range []float64{1.0478, 0.0229, -0.0501, 0.0295, 0.9905, -0.0171, -0.0092, 0.0151, 0.7521} {
		out = append(out, s15(v)...)
	}
	return out
}

// colourTags are the tags that define a matrix/TRC display profile's
// colours: what must survive byte for byte.
var colourTags = []string{"wtpt", "chad", "rXYZ", "gXYZ", "bXYZ", "rTRC", "gTRC", "bTRC"}

// displayP3 is a Display P3 matrix/TRC profile, with TRCs of trc and
// extra tags core does not keep.
func displayP3(trc func() []byte, extra ...iccTag) []byte {
	tags := []iccTag{
		{"desc", mlucTag("Display P3")},
		{"cprt", mlucTag("Copyright Apple Inc., 2017")},
		{"wtpt", xyzTag(0.96419, 1, 0.82489)},
		{"rXYZ", xyzTag(0.51512, 0.24120, -0.00105)},
		{"gXYZ", xyzTag(0.29198, 0.69225, 0.04189)},
		{"bXYZ", xyzTag(0.15710, 0.06657, 0.78407)},
		{"rTRC", trc()}, {"gTRC", trc()}, {"bTRC", trc()},
		{"chad", chadTag()},
	}
	return buildICC("mntr", "RGB ", "XYZ ", append(tags, extra...))
}

// iccTags reads the tags of a profile, with its header.
func iccTags(t *testing.T, profile []byte) map[string][]byte {
	t.Helper()
	if len(profile) < 132 || int(binary.BigEndian.Uint32(profile)) != len(profile) {
		t.Fatalf("not a profile: %d bytes", len(profile))
	}
	tags := map[string][]byte{"header": profile[:128]}
	count := int(binary.BigEndian.Uint32(profile[128:]))
	for i := 0; i < count; i++ {
		entry := 132 + 12*i
		offset := int(binary.BigEndian.Uint32(profile[entry+4:]))
		size := int(binary.BigEndian.Uint32(profile[entry+8:]))
		tags[string(profile[entry:entry+4])] = profile[offset : offset+size]
	}
	return tags
}

// requireColourTags checks a stored profile keeps the colour-defining tags
// of the uploaded one byte for byte, and nothing core does not keep.
func requireColourTags(t *testing.T, where string, stored, uploaded []byte) {
	t.Helper()
	if len(stored) == 0 {
		t.Fatalf("%s: no profile", where)
	}
	got, want := iccTags(t, stored), iccTags(t, uploaded)
	for _, sig := range colourTags {
		if !bytes.Equal(got[sig], want[sig]) {
			t.Errorf("%s: %s changed", where, sig)
		}
	}
	for sig := range got {
		switch sig {
		case "header", "desc", "cprt", "wtpt", "chad", "rXYZ", "gXYZ", "bXYZ", "rTRC", "gTRC", "bTRC":
		default:
			t.Errorf("%s: kept tag %s", where, sig)
		}
	}
	header := got["header"]
	if !bytes.Equal(header[84:100], make([]byte, 16)) || string(header[36:40]) != "acsp" || string(header[12:24]) != "mntrRGB XYZ " {
		t.Errorf("%s: header % x", where, header[:100])
	}
}

// withJPEGICC inserts a profile as APP2 ICC_PROFILE segments of at most
// 65519 bytes each, as cameras write them.
func withJPEGICC(data, profile []byte) []byte {
	var segments []byte
	count := (len(profile) + 65518) / 65519
	for i := 0; i < count; i++ {
		chunk := profile[i*65519 : min(len(profile), (i+1)*65519)]
		payload := append([]byte("ICC_PROFILE\x00"), byte(i+1), byte(count))
		payload = append(payload, chunk...)
		segments = append(segments, 0xFF, 0xE2, byte((len(payload)+2)>>8), byte(len(payload)+2))
		segments = append(segments, payload...)
	}
	out := append([]byte{}, data[:2]...)
	out = append(out, segments...)
	return append(out, data[2:]...)
}

// jpegICC reassembles the ICC profile of a JPEG from its APP2 segments.
func jpegICC(t *testing.T, data []byte) []byte {
	t.Helper()
	chunks := map[int][]byte{}
	for pos := 2; pos+4 <= len(data) && data[pos] == 0xFF && data[pos+1] != 0xDA; {
		size := int(binary.BigEndian.Uint16(data[pos+2:]))
		payload := data[pos+4 : pos+2+size]
		if data[pos+1] == 0xE2 && bytes.HasPrefix(payload, []byte("ICC_PROFILE\x00")) {
			chunks[int(payload[12])] = payload[14:]
		}
		pos += 2 + size
	}
	var order []int
	for seq := range chunks {
		order = append(order, seq)
	}
	sort.Ints(order)
	var out []byte
	for _, seq := range order {
		out = append(out, chunks[seq]...)
	}
	return out
}

// withPNGICC inserts an iCCP chunk after a PNG's header.
func withPNGICC(t *testing.T, data, profile []byte) []byte {
	t.Helper()
	var compressed bytes.Buffer
	w := zlib.NewWriter(&compressed)
	_, _ = w.Write(profile)
	_ = w.Close()
	payload := append([]byte("Display P3\x00\x00"), compressed.Bytes()...)
	chunk := binary.BigEndian.AppendUint32(nil, uint32(len(payload)))
	chunk = append(chunk, "iCCP"...)
	chunk = append(chunk, payload...)
	chunk = binary.BigEndian.AppendUint32(chunk, crc32.ChecksumIEEE(append([]byte("iCCP"), payload...)))
	out := append([]byte{}, data[:33]...)
	out = append(out, chunk...)
	return append(out, data[33:]...)
}

// pngICC is the profile of a PNG's iCCP chunk, inflated.
func pngICC(t *testing.T, data []byte) []byte {
	t.Helper()
	for pos := 8; pos+8 <= len(data); {
		size := int(binary.BigEndian.Uint32(data[pos:]))
		if string(data[pos+4:pos+8]) == "iCCP" {
			payload := data[pos+8 : pos+8+size]
			at := bytes.IndexByte(payload, 0)
			r, err := zlib.NewReader(bytes.NewReader(payload[at+2:]))
			if err != nil {
				t.Fatal(err)
			}
			profile, err := io.ReadAll(r)
			if err != nil {
				t.Fatal(err)
			}
			return profile
		}
		pos += 12 + size
	}
	return nil
}

// A wide-gamut photo keeps its colours: its profile is rebuilt from the
// tags that define them, byte for byte, in the re-encoded image and in
// every size. What the profile carries besides (here a 150 KB lookup
// table core does not keep) goes.
func TestService_PurposeKeepsTheColourTagsOfADisplayP3JPEG(t *testing.T) {
	t.Parallel()
	svc, blobs := setup(t)
	// Sampled curves long enough that the rebuilt profile takes two APP2
	// segments, and a table that makes the uploaded one take three.
	profile := displayP3(func() []byte { return curvTag(12_000) }, iccTag{"A2B0", bytes.Repeat([]byte{7}, 150_000)}, iccTag{"zzzz", []byte("zzzz0000opaque")})
	photo := withJPEGICC(solidJPEG(t, 1600, 1200, color.RGBA{R: 250, G: 20, B: 20, A: 255}), profile)

	created, err := svc.UploadForPurpose(context.Background(), signedIn("90909090-9090-9090-9090-909090909090"), "profile_picture", uploaded("p3.jpg", "image/jpeg", photo))
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{created.Key, created.Key + "/card.jpg", created.Key + "/page.jpg"} {
		stored, ok := blobs.Get(key)
		if !ok {
			t.Fatalf("no object at %s", key)
		}
		requireColourTags(t, key, jpegICC(t, stored), profile)
		decodeStored(t, stored)
	}
}

func TestService_PurposeKeepsTheColourTagsOfAPNG(t *testing.T) {
	t.Parallel()
	svc, blobs := setup(t)
	profile := displayP3(paraTag)
	upload := withPNGICC(t, solidPNG(t, 1600, 1200, color.RGBA{G: 200, A: 255}), profile)

	created, err := svc.UploadForPurpose(context.Background(), signedIn("91919191-9191-9191-9191-919191919191"), "profile_picture", uploaded("p3.png", "image/png", upload))
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{created.Key, created.Key + "/card.png", created.Key + "/page.png"} {
		stored, _ := blobs.Get(key)
		requireColourTags(t, key, pngICC(t, stored), profile)
		decodeStored(t, stored)
	}
}

// A profile core cannot rebuild is dropped, and the image is then taken
// as sRGB: a CMYK or grey profile, one without its matrix and curves
// (lookup tables only), a tag out of the profile's bounds, no 'acsp'
// signature, or above 1 MiB.
func TestService_PurposeDropsAColourProfileItCannotRebuild(t *testing.T) {
	t.Parallel()
	svc, blobs := setup(t)
	outOfBounds := displayP3(paraTag)
	binary.BigEndian.PutUint32(outOfBounds[132+12*3+8:], 1<<20) // rXYZ's size
	unsigned := displayP3(paraTag)
	copy(unsigned[36:], "xxxx")
	for name, profile := range map[string][]byte{
		"CMYK":                buildICC("prtr", "CMYK", "Lab ", []iccTag{{"desc", mlucTag("CMYK")}, {"A2B0", bytes.Repeat([]byte{1}, 64)}}),
		"grey":                buildICC("mntr", "GRAY", "XYZ ", []iccTag{{"desc", mlucTag("Grey")}, {"wtpt", xyzTag(0.96419, 1, 0.82489)}, {"kTRC", paraTag()}}),
		"lookup tables only":  buildICC("mntr", "RGB ", "XYZ ", []iccTag{{"desc", mlucTag("LUT")}, {"wtpt", xyzTag(0.96419, 1, 0.82489)}, {"A2B0", bytes.Repeat([]byte{1}, 64)}}),
		"a tag out of bounds": outOfBounds,
		"no signature":        unsigned,
		"above 1 MiB":         displayP3(paraTag, iccTag{"A2B0", make([]byte, 1<<20)}),
	} {
		photo := withJPEGICC(solidJPEG(t, 64, 48, color.RGBA{B: 200, A: 255}), profile)
		created, err := svc.UploadForPurpose(context.Background(), signedIn("92929292-9292-9292-9292-929292929292"), "profile_picture", uploaded("x.jpg", "image/jpeg", photo))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		stored, _ := blobs.Get(created.Key)
		if bytes.Contains(stored, []byte("ICC_PROFILE")) {
			t.Errorf("%s: a profile was kept", name)
		}
	}
}
