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
	for i := 0; i < len(img.Pix); i += 4 {
		img.Pix[i] = 0
		img.Pix[i+1] = 0
		img.Pix[i+2] = 0
	}
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
