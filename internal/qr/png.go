package qr

import (
	"bytes"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"os"
	"strconv"
	"strings"

	qrcode "github.com/skip2/go-qrcode"
)

const DefaultSize = 256

func PNG(content string, size int) ([]byte, error) {
	if size < 64 || size > 1024 {
		size = DefaultSize
	}
	return qrcode.Encode(content, qrcode.Medium, size)
}

func PNGWithLogo(content string, size int) ([]byte, error) {
	if size < 64 || size > 1024 {
		size = DefaultSize
	}
	code, err := qrcode.New(content, qrcode.Highest)
	if err != nil {
		return nil, err
	}
	code.DisableBorder = false
	qrPNG, err := code.PNG(size)
	if err != nil {
		return nil, err
	}
	return overlayLogo(qrPNG)
}

func overlayLogo(qrPNG []byte) ([]byte, error) {
	src, err := png.Decode(bytes.NewReader(qrPNG))
	if err != nil {
		return nil, err
	}
	bounds := src.Bounds()
	dst := image.NewRGBA(bounds)
	draw.Draw(dst, bounds, src, bounds.Min, draw.Src)

	logoSize := bounds.Dx() / 5
	if logoSize < 16 {
		logoSize = 16
	}
	logo := clubLogo(logoSize)
	x := bounds.Min.X + (bounds.Dx()-logoSize)/2
	y := bounds.Min.Y + (bounds.Dy()-logoSize)/2
	pad := logoSize / 10
	bg := image.Rect(x-pad, y-pad, x+logoSize+pad, y+logoSize+pad)
	draw.Draw(dst, bg, &image.Uniform{C: color.White}, image.Point{}, draw.Src)
	draw.Draw(dst, image.Rect(x, y, x+logoSize, y+logoSize), logo, image.Point{}, draw.Over)

	var buf bytes.Buffer
	if err := png.Encode(&buf, dst); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func clubLogo(size int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, size, size))
	black := color.RGBA{A: 255}
	white := color.RGBA{R: 255, G: 255, B: 255, A: 255}
	draw.Draw(img, img.Bounds(), &image.Uniform{C: black}, image.Point{}, draw.Src)
	inset := size / 6
	inner := image.Rect(inset, inset, size-inset, size-inset)
	draw.Draw(img, inner, &image.Uniform{C: white}, image.Point{}, draw.Src)
	core := size / 3
	mid := image.Rect(core, core, size-core, size-core)
	draw.Draw(img, mid, &image.Uniform{C: black}, image.Point{}, draw.Src)
	return img
}

func SizeFromQuery(raw string) int {
	if raw == "" {
		return DefaultSize
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return DefaultSize
	}
	return n
}

func ShortURL(alias string) string {
	origin := strings.TrimRight(os.Getenv("SHORT_ORIGIN"), "/")
	if origin == "" {
		origin = "https://skyl.app"
	}
	return origin + "/" + alias
}

func LogoFromQuery(raw string) bool {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "true", "yes":
		return true
	default:
		return false
	}
}
