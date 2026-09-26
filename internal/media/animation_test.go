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
