package media_test

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"reflect"
	"testing"

	"github.com/skylab-kulubu/core-backend/internal/media"
)

func twoTonePNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, 120, 1))
	for x := 0; x < 120; x++ {
		c := color.NRGBA{R: 138, G: 100, B: 47, A: 255}
		if x >= 60 {
			c = color.NRGBA{R: 60, G: 130, B: 190, A: 255}
		}
		img.SetNRGBA(x, 0, c)
	}
	var out bytes.Buffer
	if err := png.Encode(&out, img); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func TestExtractCoverColorsMatchesAppOrdering(t *testing.T) {
	t.Parallel()
	got := media.ExtractCoverColors(twoTonePNG(t))
	want := []string{"#3c82be", "#8a642f"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("colors %#v want %#v", got, want)
	}
	if again := media.ExtractCoverColors(twoTonePNG(t)); !reflect.DeepEqual(again, got) {
		t.Fatalf("not deterministic: %#v then %#v", got, again)
	}
}

func TestExtractCoverColorsInvalidImageIsEmpty(t *testing.T) {
	t.Parallel()
	if got := media.ExtractCoverColors([]byte("not an image")); len(got) != 0 {
		t.Fatalf("colors %#v", got)
	}
}
