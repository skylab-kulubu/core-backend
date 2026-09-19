package qr

import (
	"bytes"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"strings"
	"testing"
)

func TestPNGEncodesContent(t *testing.T) {
	t.Parallel()
	png, err := PNG("https://skyl.app/club", 128)
	if err != nil {
		t.Fatal(err)
	}
	if len(png) < 8 || !bytes.Equal(png[:8], []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}) {
		t.Fatalf("not a png, len=%d", len(png))
	}
}

func TestSizeFromQuery(t *testing.T) {
	t.Parallel()
	if SizeFromQuery("") != DefaultSize {
		t.Fatal("empty")
	}
	if SizeFromQuery("512") != 512 {
		t.Fatal("512")
	}
	if SizeFromQuery("nope") != DefaultSize {
		t.Fatal("invalid")
	}
}

func TestPNGWithLogoEncodesContent(t *testing.T) {
	t.Parallel()
	plain, err := PNG("https://skyl.app/club", 128)
	if err != nil {
		t.Fatal(err)
	}
	withLogo, err := PNGWithLogo("https://skyl.app/club", 128)
	if err != nil {
		t.Fatal(err)
	}
	if len(withLogo) < 8 || !bytes.Equal(withLogo[:8], []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}) {
		t.Fatalf("not a png, len=%d", len(withLogo))
	}
	if bytes.Equal(plain, withLogo) {
		t.Fatal("logo overlay should change the png")
	}
}

func TestClubLogoUsesTransparentBrandMark(t *testing.T) {
	t.Parallel()
	logo := clubLogo(128)
	_, _, _, cornerAlpha := logo.At(0, 0).RGBA()
	centerRed, centerGreen, centerBlue, centerAlpha := logo.At(64, 64).RGBA()
	if cornerAlpha != 0 {
		t.Fatalf("brand mark corner alpha = %d", cornerAlpha)
	}
	if centerAlpha == 0 || centerRed != 0 || centerGreen != 0 || centerBlue != 0 {
		t.Fatalf("brand mark center rgba = %d,%d,%d,%d", centerRed, centerGreen, centerBlue, centerAlpha)
	}
}

func TestCropTransparentRemovesEmptyOuterRows(t *testing.T) {
	t.Parallel()
	src := image.NewNRGBA(image.Rect(0, 0, 10, 10))
	src.Set(2, 3, color.Black)
	src.Set(7, 8, color.Black)

	got := cropTransparent(src)
	if got.Bounds() != image.Rect(2, 3, 8, 9) {
		t.Fatalf("cropped bounds = %v", got.Bounds())
	}
}

func TestOverlayLogoAlignsBackgroundToModuleGrid(t *testing.T) {
	t.Parallel()
	src := image.NewRGBA(image.Rect(0, 0, 100, 100))
	draw.Draw(src, src.Bounds(), &image.Uniform{C: color.RGBA{A: 255}}, image.Point{}, draw.Src)
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, src); err != nil {
		t.Fatal(err)
	}
	overlaid, err := overlayLogo(encoded.Bytes(), 25)
	if err != nil {
		t.Fatal(err)
	}
	got, err := png.Decode(bytes.NewReader(overlaid))
	if err != nil {
		t.Fatal(err)
	}
	black := color.RGBA{A: 255}
	white := color.RGBA{R: 255, G: 255, B: 255, A: 255}
	if pixel := color.RGBAModel.Convert(got.At(31, 50)).(color.RGBA); pixel != black {
		t.Fatalf("pixel before aligned panel = %#v want %#v", pixel, black)
	}
	if pixel := color.RGBAModel.Convert(got.At(32, 32)).(color.RGBA); pixel != white {
		t.Fatalf("aligned panel corner = %#v want %#v", pixel, white)
	}
	if pixel := color.RGBAModel.Convert(got.At(68, 50)).(color.RGBA); pixel != black {
		t.Fatalf("pixel after aligned panel = %#v want %#v", pixel, black)
	}
}

func TestLogoFromQuery(t *testing.T) {
	t.Parallel()
	if LogoFromQuery("") || LogoFromQuery("0") {
		t.Fatal("empty")
	}
	if !LogoFromQuery("1") || !LogoFromQuery("true") {
		t.Fatal("true")
	}
}

func TestSessionURL(t *testing.T) {
	t.Parallel()
	id := "11111111-1111-1111-1111-111111111111"
	got := SessionURL(id)
	if !strings.HasSuffix(got, "/v1/sessions/"+id) {
		t.Fatalf("got %s", got)
	}
}
