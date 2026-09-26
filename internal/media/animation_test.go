package media_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"image"
	"image/color"
	"image/gif"
	"image/png"
	"testing"

	"github.com/skylab-kulubu/core-backend/internal/media"
)

var gifPalette = color.Palette{color.RGBA{R: 230, A: 255}, color.RGBA{B: 230, A: 255}, color.RGBA{G: 230, A: 255}, color.RGBA{}}

// gifFrame is a frame of one palette colour over rect.
func gifFrame(rect image.Rectangle, index uint8) *image.Paletted {
	img := image.NewPaletted(rect, gifPalette)
	for i := range img.Pix {
		img.Pix[i] = index
	}
	return img
}

// withGIFComment inserts a comment extension after the logical screen and
// its colour table, as an editor writes one.
func withGIFComment(t *testing.T, data []byte, comment string) []byte {
	t.Helper()
	at := 13
	if data[10]&0x80 != 0 {
		at += 3 * (1 << (int(data[10]&0x07) + 1))
	}
	ext := append([]byte{0x21, 0xFE, byte(len(comment))}, comment...)
	ext = append(ext, 0x00)
	out := append([]byte{}, data[:at]...)
	out = append(out, ext...)
	return append(out, data[at:]...)
}

// An animated GIF is re-encoded frame by frame: its frames, delays,
// disposal and loop count stay; its comments go. Its sizes are the first
// frame, still, as PNG.
func TestService_PurposeKeepsAnAnimatedGIFAnimated(t *testing.T) {
	t.Parallel()
	svc, blobs := setup(t)
	var animated bytes.Buffer
	if err := gif.EncodeAll(&animated, &gif.GIF{
		Image: []*image.Paletted{
			gifFrame(image.Rect(0, 0, 800, 600), 0),
			gifFrame(image.Rect(100, 100, 300, 200), 1),
			gifFrame(image.Rect(0, 0, 800, 600), 2),
		},
		Delay:     []int{10, 20, 30},
		Disposal:  []byte{gif.DisposalNone, gif.DisposalBackground, gif.DisposalPrevious},
		LoopCount: 3,
		Config:    image.Config{ColorModel: gifPalette, Width: 800, Height: 600},
	}); err != nil {
		t.Fatal(err)
	}
	upload := withGIFComment(t, animated.Bytes(), "SECRET-COMMENT")

	created, err := svc.UploadForPurpose(context.Background(), organizer(), "event_gallery", uploaded("wave.gif", "image/gif", upload))
	if err != nil {
		t.Fatal(err)
	}
	stored, _ := blobs.Get(created.Key)
	if bytes.Contains(stored, []byte("SECRET-COMMENT")) {
		t.Fatal("the GIF kept its comment")
	}
	got, err := gif.DecodeAll(bytes.NewReader(stored))
	if err != nil {
		t.Fatalf("stored GIF: %v", err)
	}
	if created.Type != "image/gif" || len(got.Image) != 3 || got.LoopCount != 3 || got.Config.Width != 800 || got.Config.Height != 600 {
		t.Fatalf("stored %s: %d frames, loop %d, %dx%d", created.Type, len(got.Image), got.LoopCount, got.Config.Width, got.Config.Height)
	}
	if got.Delay[0] != 10 || got.Delay[1] != 20 || got.Delay[2] != 30 ||
		got.Disposal[0] != gif.DisposalNone || got.Disposal[1] != gif.DisposalBackground || got.Disposal[2] != gif.DisposalPrevious {
		t.Fatalf("delays %v, disposal %v", got.Delay, got.Disposal)
	}
	if got.Image[1].Bounds() != image.Rect(100, 100, 300, 200) || !isRed(got.Image[0].At(10, 10)) {
		t.Fatalf("frames changed: %v, %v", got.Image[1].Bounds(), got.Image[0].At(10, 10))
	}
	card, ok := blobs.Get(created.Key + "/card.png")
	if !ok {
		t.Fatalf("no card size: %v", blobs.Keys())
	}
	still, err := png.Decode(bytes.NewReader(card))
	if err != nil || still.Bounds().Size() != image.Pt(400, 300) || !isRed(still.At(200, 150)) {
		t.Fatalf("card %v %v, want the first frame at 400×300", err, still)
	}
	if created.Width != 800 || created.Height != 600 {
		t.Fatalf("recorded %d×%d", created.Width, created.Height)
	}
}

// craftedGIF is a GIF of frames frames of w×h over a w×h screen, each with
// a few bytes of image data: its frame count and sizes cost its sender
// almost nothing to claim.
func craftedGIF(screenW, screenH, frameW, frameH, frames int) []byte {
	le := binary.LittleEndian
	out := []byte("GIF89a")
	out = le.AppendUint16(out, uint16(screenW))
	out = le.AppendUint16(out, uint16(screenH))
	out = append(out, 0x00, 0x00, 0x00)
	for i := 0; i < frames; i++ {
		out = append(out, 0x2C)
		out = le.AppendUint16(out, 0)
		out = le.AppendUint16(out, 0)
		out = le.AppendUint16(out, uint16(frameW))
		out = le.AppendUint16(out, uint16(frameH))
		out = append(out, 0x80, 0, 0, 0, 255, 255, 255) // a local table of two colours
		out = append(out, 0x02, 0x02, 0x4C, 0x01, 0x00)
	}
	return append(out, 0x3B)
}

func TestService_PurposeRefusesAGIFBeyondItsFrameLimits(t *testing.T) {
	svc, blobs := setup(t)
	p := organizer()
	for name, tc := range map[string]struct {
		data []byte
		want error
	}{
		"more than 300 frames":             {craftedGIF(8, 8, 8, 8, 301), media.ErrTypeNotAllowed},
		"a frame larger than its screen":   {craftedGIF(8, 8, 16, 8, 2), media.ErrTypeNotAllowed},
		"frames × screen above the budget": {craftedGIF(4000, 4000, 4000, 4000, 20), media.ErrImageTooLarge},
	} {
		var err error
		grew := allocated(func() {
			_, err = svc.UploadForPurpose(context.Background(), p, "event_gallery", uploaded("x.gif", "image/gif", tc.data))
		})
		if !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", name, err, tc.want)
		}
		if grew > 32<<20 {
			t.Errorf("%s: allocated %d MiB", name, grew>>20)
		}
	}
	if keys := blobs.Keys(); len(keys) != 0 {
		t.Fatalf("stored %v", keys)
	}
}

// webpChunk is a RIFF chunk, padded to an even length.
func webpChunk(fourcc string, payload []byte) []byte {
	out := append([]byte(fourcc), binary.LittleEndian.AppendUint32(nil, uint32(len(payload)))...)
	out = append(out, payload...)
	if len(payload)%2 == 1 {
		out = append(out, 0)
	}
	return out
}

func uint24(v int) []byte { return []byte{byte(v), byte(v >> 8), byte(v >> 16)} }

// oneByOneVP8L is the VP8L bitstream of a 1×1 lossless image (from
// Modernizr's WebP feature test).
var oneByOneVP8L = []byte{0x2f, 0x00, 0x00, 0x00, 0x10, 0x07, 0x10, 0x11, 0x11, 0x88, 0x88, 0xfe, 0x07, 0x00}

// animatedWebP is an animated WebP over a w×h canvas with a 1×1 frame at
// each of the given places, and EXIF and XMP chunks that name the uploader
// and a place.
func animatedWebP(w, h int, frames []image.Point, extra ...[]byte) []byte {
	return animatedWebPOf(w, h, frames, oneByOneVP8L, extra...)
}

// animatedWebPOf is animatedWebP with every frame the given VP8L bitstream.
func animatedWebPOf(w, h int, frames []image.Point, bitstream []byte, extra ...[]byte) []byte {
	vp8x := append([]byte{0x02 | 0x08 | 0x04, 0, 0, 0}, uint24(w-1)...)
	vp8x = append(vp8x, uint24(h-1)...)
	body := []byte("WEBP")
	body = append(body, webpChunk("VP8X", vp8x)...)
	body = append(body, webpChunk("ANIM", []byte{0, 0, 0, 0, 0, 0})...)
	for _, at := range frames {
		anmf := append(uint24(at.X/2), uint24(at.Y/2)...)
		anmf = append(anmf, uint24(0)...) // width-1
		anmf = append(anmf, uint24(0)...) // height-1
		anmf = append(anmf, uint24(100)...)
		anmf = append(anmf, 0)
		anmf = append(anmf, webpChunk("VP8L", bitstream)...)
		body = append(body, webpChunk("ANMF", anmf)...)
	}
	body = append(body, webpChunk("EXIF", []byte("Exif\x00\x00GPS-SECRET"))...)
	body = append(body, webpChunk("XMP ", []byte("<x:xmpmeta>uploader</x:xmpmeta>"))...)
	for _, chunk := range extra {
		body = append(body, chunk...)
	}
	return append(append([]byte("RIFF"), binary.LittleEndian.AppendUint32(nil, uint32(len(body)))...), body...)
}

// An animated WebP cannot be re-encoded in Go: after its structure is
// checked it is kept as uploaded, without its EXIF and XMP, and without
// sizes (every size is the image itself).
func TestService_PurposeKeepsAValidAnimatedWebPAsUploaded(t *testing.T) {
	t.Parallel()
	svc, blobs := setup(t)
	upload := animatedWebP(4, 4, []image.Point{{0, 0}, {2, 2}})

	created, err := svc.UploadForPurpose(context.Background(), organizer(), "event_gallery", uploaded("wave.webp", "image/webp", upload))
	if err != nil {
		t.Fatal(err)
	}
	stored, _ := blobs.Get(created.Key)
	for _, gone := range []string{"GPS-SECRET", "EXIF", "XMP ", "uploader"} {
		if bytes.Contains(stored, []byte(gone)) {
			t.Errorf("the stored WebP still carries %q", gone)
		}
	}
	if string(stored[:4]) != "RIFF" || int(binary.LittleEndian.Uint32(stored[4:]))+8 != len(stored) {
		t.Fatalf("the RIFF size does not match the file (%d bytes)", len(stored))
	}
	if stored[20]&0x02 == 0 || stored[20]&0x0C != 0 {
		t.Fatalf("VP8X flags %08b, want animation without EXIF or XMP", stored[20])
	}
	if bytes.Count(stored, []byte("ANMF")) != 2 || !bytes.Contains(stored, oneByOneVP8L) {
		t.Fatal("the frames changed")
	}
	if created.Type != "image/webp" || created.Width != 4 || created.Height != 4 || len(blobs.Keys()) != 1 {
		t.Fatalf("created %s %d×%d, objects %v", created.Type, created.Width, created.Height, blobs.Keys())
	}
	if card := created.Sizes["card"]; card.URL != created.URL {
		t.Fatalf("card %+v, want the WebP itself", card)
	}
}

func TestService_PurposeRefusesAnAnimatedWebPWithABadStructure(t *testing.T) {
	t.Parallel()
	svc, blobs := setup(t)
	manyFrames := make([]image.Point, 301)
	for name, tc := range map[string]struct {
		data []byte
		want error
	}{
		"a frame outside the canvas":  {animatedWebP(4, 4, []image.Point{{4, 0}}), media.ErrTypeNotAllowed},
		"more than 300 frames":        {animatedWebP(4, 4, manyFrames), media.ErrTypeNotAllowed},
		"frames × canvas over budget": {animatedWebP(2560, 2560, make([]image.Point, 50)), media.ErrImageTooLarge},
		"an unknown chunk":            {animatedWebP(4, 4, []image.Point{{0, 0}}, webpChunk("ABCD", []byte("hi"))), media.ErrTypeNotAllowed},
		"trailing data":               {append(animatedWebP(4, 4, []image.Point{{0, 0}}), []byte("<html>trailer</html>")...), media.ErrTypeNotAllowed},
		"a truncated file":            {animatedWebP(4, 4, []image.Point{{0, 0}})[:40], media.ErrTypeNotAllowed},
		// The bitstream says 1×1, as its frame does, but does not decode.
		"a frame that does not decode": {animatedWebPOf(4, 4, []image.Point{{0, 0}}, append(append([]byte{}, oneByOneVP8L[:5]...), 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff)), media.ErrTypeNotAllowed},
	} {
		_, err := svc.UploadForPurpose(context.Background(), organizer(), "event_gallery", uploaded("x.webp", "image/webp", tc.data))
		if !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", name, err, tc.want)
		}
	}
	if keys := blobs.Keys(); len(keys) != 0 {
		t.Fatalf("stored %v", keys)
	}
}

// withICCP puts an ICCP chunk after an animated WebP's VP8X chunk and sets
// its ICC flag.
func withICCP(data, profile []byte) []byte {
	body := append([]byte{}, data[12:12+18]...) // VP8X
	body[8] |= 0x20
	body = append(body, webpChunk("ICCP", profile)...)
	body = append(body, data[12+18:]...)
	out := append([]byte("RIFFxxxxWEBP"), body...)
	binary.LittleEndian.PutUint32(out[4:], uint32(len(out)-8))
	return out
}

// An animated WebP keeps its colours the way a re-encoded image does: its
// profile rebuilt, or dropped with the VP8X ICC flag when it cannot be.
func TestService_AnimatedWebPKeepsARebuiltColourProfile(t *testing.T) {
	t.Parallel()
	svc, blobs := setup(t)
	profile := displayP3(paraTag)
	for name, tc := range map[string]struct {
		profile []byte
		kept    bool
	}{
		"Display P3": {profile, true},
		"CMYK":       {buildICC("prtr", "CMYK", "Lab ", []iccTag{{"desc", mlucTag("CMYK")}}), false},
	} {
		created, err := svc.UploadForPurpose(context.Background(), organizer(), "event_gallery", uploaded("wave.webp", "image/webp", withICCP(animatedWebP(4, 4, []image.Point{{0, 0}}), tc.profile)))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		stored, _ := blobs.Get(created.Key)
		at := bytes.Index(stored, []byte("ICCP"))
		if flag := stored[20]&0x20 != 0; flag != tc.kept || (at >= 0) != tc.kept {
			t.Fatalf("%s: ICC flag %v, chunk at %d; want kept %v", name, flag, at, tc.kept)
		}
		if tc.kept {
			size := int(binary.LittleEndian.Uint32(stored[at+4:]))
			requireColourTags(t, name, stored[at+8:at+8+size], profile)
		}
	}
}
