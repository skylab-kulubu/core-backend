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

// iccProfile is an ICC profile of the given length that names itself
// Display P3: a v4 display profile header ('acsp' at 36) and a desc tag,
// padded.
func iccProfile(length int) []byte {
	out := make([]byte, length)
	binary.BigEndian.PutUint32(out[0:], uint32(length))
	copy(out[4:], "appl")
	binary.BigEndian.PutUint32(out[8:], 0x04400000)
	copy(out[12:], "mntrRGB XYZ ")
	copy(out[36:], "acsp")
	binary.BigEndian.PutUint32(out[128:], 1)
	copy(out[132:], "desc")
	binary.BigEndian.PutUint32(out[136:], 144)
	binary.BigEndian.PutUint32(out[140:], 32)
	copy(out[144:], "desc\x00\x00\x00\x00Display P3")
	for i := 176; i < length; i++ {
		out[i] = byte(i)
	}
	return out
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

// A wide-gamut photo keeps its colour profile, byte for byte, in the
// re-encoded image and in every size: no colour conversion, no profile
// dropped.
func TestService_PurposeKeepsTheColourProfileOfAJPEG(t *testing.T) {
	t.Parallel()
	svc, blobs := setup(t)
	profile := iccProfile(150_000) // three APP2 segments
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
		if got := jpegICC(t, stored); !bytes.Equal(got, profile) {
			t.Errorf("%s: profile of %d bytes, want the %d uploaded", key, len(got), len(profile))
		}
		decodeStored(t, stored)
	}
}

func TestService_PurposeKeepsTheColourProfileOfAPNG(t *testing.T) {
	t.Parallel()
	svc, blobs := setup(t)
	profile := iccProfile(3_000)
	upload := withPNGICC(t, solidPNG(t, 1600, 1200, color.RGBA{G: 200, A: 255}), profile)

	created, err := svc.UploadForPurpose(context.Background(), signedIn("91919191-9191-9191-9191-919191919191"), "profile_picture", uploaded("p3.png", "image/png", upload))
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{created.Key, created.Key + "/card.png", created.Key + "/page.png"} {
		stored, _ := blobs.Get(key)
		if got := pngICC(t, stored); !bytes.Equal(got, profile) {
			t.Errorf("%s: profile of %d bytes, want the %d uploaded", key, len(got), len(profile))
		}
		decodeStored(t, stored)
	}
}

// A profile that is not one (no 'acsp' signature), or one above 1 MiB, is
// dropped rather than carried.
func TestService_PurposeDropsAnInvalidColourProfile(t *testing.T) {
	t.Parallel()
	svc, blobs := setup(t)
	notAProfile := iccProfile(2_000)
	copy(notAProfile[36:], "xxxx")
	for name, profile := range map[string][]byte{"no signature": notAProfile, "above 1 MiB": iccProfile(1<<20 + 1)} {
		photo := withJPEGICC(solidJPEG(t, 64, 48, color.RGBA{B: 200, A: 255}), profile)
		created, err := svc.UploadForPurpose(context.Background(), signedIn("92929292-9292-9292-9292-929292929292"), "profile_picture", uploaded("x.jpg", "image/jpeg", photo))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		stored, _ := blobs.Get(created.Key)
		if bytes.Contains(stored, []byte("ICC_PROFILE")) {
			t.Errorf("%s: the profile was kept", name)
		}
	}
}
