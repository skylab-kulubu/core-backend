package qr

import (
	"strconv"
	"strings"

	qrcode "github.com/skip2/go-qrcode"
)

// plainQuietZone is the four-module margin the QR standard asks for: a
// screen is scanned from further away, and at an angle, than a poster.
const plainQuietZone = 4

// PlainSVG draws the code with square modules and no mark: one path whose
// subpaths are the runs of dark modules in each row. It is what a screen
// that redraws the code every few seconds needs: small and quick to make.
func PlainSVG(content string) ([]byte, error) {
	code, err := qrcode.New(content, qrcode.Medium)
	if err != nil {
		return nil, err
	}
	code.DisableBorder = true
	bitmap := code.Bitmap()
	side := strconv.Itoa(len(bitmap) + 2*plainQuietZone)
	var b strings.Builder
	b.WriteString(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 ` + side + " " + side + `" shape-rendering="crispEdges">`)
	b.WriteString(`<rect width="` + side + `" height="` + side + `" fill="#fff"/><path fill="#000" d="`)
	for y, row := range bitmap {
		for x := 0; x < len(row); {
			if !row[x] {
				x++
				continue
			}
			run := 1
			for x+run < len(row) && row[x+run] {
				run++
			}
			w := strconv.Itoa(run)
			b.WriteString("M" + strconv.Itoa(x+plainQuietZone) + " " + strconv.Itoa(y+plainQuietZone) + "h" + w + "v1h-" + w + "z")
			x += run
		}
	}
	b.WriteString(`"/></svg>`)
	return []byte(b.String()), nil
}
