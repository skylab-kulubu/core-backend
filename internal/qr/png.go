package qr

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"os"
	"strconv"
	"strings"
	"sync"

	qrcode "github.com/skip2/go-qrcode"
	xdraw "golang.org/x/image/draw"
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
	draw.Draw(dst, image.Rect(x, y, x+logoSize, y+logoSize), logo, image.Point{}, draw.Over)

	var buf bytes.Buffer
	if err := png.Encode(&buf, dst); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

var (
	logoOnce   sync.Once
	logoSource image.Image
)

func clubLogo(size int) *image.NRGBA {
	logoOnce.Do(func() {
		raw, err := base64.StdEncoding.DecodeString(logoAssetBase64)
		if err != nil {
			panic(err)
		}
		logoSource, err = png.Decode(bytes.NewReader(raw))
		if err != nil {
			panic(err)
		}
	})
	img := image.NewNRGBA(image.Rect(0, 0, size, size))
	xdraw.CatmullRom.Scale(img, img.Bounds(), logoSource, logoSource.Bounds(), xdraw.Src, nil)
	for y := 0; y < size; y++ {
		brand := logoGradient(y, size)
		for x := 0; x < size; x++ {
			i := y*img.Stride + x*4
			img.Pix[i] = brand.R
			img.Pix[i+1] = brand.G
			img.Pix[i+2] = brand.B
		}
	}
	return img
}

func logoGradient(y, size int) color.NRGBA {
	position := 0.5
	if size > 1 {
		position = float64(y) / float64(size-1)
	}
	blue := color.NRGBA{R: 0x06, G: 0x99, B: 0xda, A: 0xff}
	purple := color.NRGBA{R: 0x7b, G: 0x4c, B: 0x84, A: 0xff}
	red := color.NRGBA{R: 0xe1, G: 0x06, B: 0x35, A: 0xff}
	switch {
	case position <= 0.25:
		return blue
	case position < 0.5:
		return mixLogoColor(blue, purple, (position-0.25)/0.25)
	case position < 0.75:
		return mixLogoColor(purple, red, (position-0.5)/0.25)
	default:
		return red
	}
}

func mixLogoColor(a, b color.NRGBA, amount float64) color.NRGBA {
	mix := func(left, right uint8) uint8 {
		return uint8(float64(left) + (float64(right)-float64(left))*amount + 0.5)
	}
	return color.NRGBA{R: mix(a.R, b.R), G: mix(a.G, b.G), B: mix(a.B, b.B), A: 0xff}
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

func SessionURL(id string) string {
	origin := strings.TrimRight(os.Getenv("PUBLIC_API_ORIGIN"), "/")
	if origin == "" {
		origin = "https://api.yildizskylab.com"
	}
	return origin + "/v1/sessions/" + id
}

func LogoFromQuery(raw string) bool {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "true", "yes":
		return true
	default:
		return false
	}
}
