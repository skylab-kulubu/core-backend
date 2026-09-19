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
	return overlayLogo(qrPNG, len(code.Bitmap()))
}

func overlayLogo(qrPNG []byte, moduleCount int) ([]byte, error) {
	src, err := png.Decode(bytes.NewReader(qrPNG))
	if err != nil {
		return nil, err
	}
	bounds := src.Bounds()
	dst := image.NewRGBA(bounds)
	draw.Draw(dst, bounds, src, bounds.Min, draw.Src)

	badgeModules := 9
	if moduleCount < badgeModules+2 {
		badgeModules = moduleCount - 2
		if badgeModules%2 == 0 {
			badgeModules--
		}
	}
	startModule := (moduleCount - badgeModules) / 2
	endModule := startModule + badgeModules
	badge := image.Rect(
		bounds.Min.X+moduleBoundary(startModule, bounds.Dx(), moduleCount),
		bounds.Min.Y+moduleBoundary(startModule, bounds.Dy(), moduleCount),
		bounds.Min.X+moduleBoundary(endModule, bounds.Dx(), moduleCount),
		bounds.Min.Y+moduleBoundary(endModule, bounds.Dy(), moduleCount),
	)
	draw.Draw(dst, badge, &image.Uniform{C: color.White}, image.Point{}, draw.Src)

	logoRect := badge
	logo := clubLogo(logoRect.Dx())
	draw.Draw(dst, logoRect, logo, image.Point{}, draw.Over)

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
		decoded, err := png.Decode(bytes.NewReader(raw))
		if err != nil {
			panic(err)
		}
		logoSource = cropTransparent(decoded)
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

func cropTransparent(src image.Image) image.Image {
	bounds := src.Bounds()
	minX, minY := bounds.Max.X, bounds.Max.Y
	maxX, maxY := bounds.Min.X-1, bounds.Min.Y-1
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			_, _, _, alpha := src.At(x, y).RGBA()
			if alpha <= 0x0808 {
				continue
			}
			minX = min(minX, x)
			minY = min(minY, y)
			maxX = max(maxX, x)
			maxY = max(maxY, y)
		}
	}
	if maxX < minX || maxY < minY {
		return src
	}
	return src.(interface {
		SubImage(image.Rectangle) image.Image
	}).SubImage(image.Rect(minX, minY, maxX+1, maxY+1))
}

func moduleBoundary(module, pixels, moduleCount int) int {
	return (module*pixels + moduleCount - 1) / moduleCount
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
