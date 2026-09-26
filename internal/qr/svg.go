package qr

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"image/png"
	"strings"
	"sync"

	qrcode "github.com/skip2/go-qrcode"
)

var (
	markOnce sync.Once
	markData string
	markErr  error
)

// SVG draws the code as vector modules so a poster stays sharp at any print
// size. The logo badge matches PNGWithLogo: same modules cleared, same black
// mark, embedded as an image because the club mark only exists as a PNG.
func SVG(content string, logo bool) ([]byte, error) {
	level := qrcode.Medium
	if logo {
		level = qrcode.Highest
	}
	code, err := qrcode.New(content, level)
	if err != nil {
		return nil, err
	}
	code.DisableBorder = false
	bitmap := code.Bitmap()
	n := len(bitmap)
	var b strings.Builder
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %d %d">`, n, n)
	fmt.Fprintf(&b, `<rect width="%d" height="%d" fill="#fff"/><path fill="#000" shape-rendering="crispEdges" d="`, n, n)
	for y, row := range bitmap {
		for x, dark := range row {
			if dark {
				fmt.Fprintf(&b, "M%d %dh1v1h-1z", x, y)
			}
		}
	}
	b.WriteString(`"/>`)
	if logo {
		mark, err := logoMark()
		if err != nil {
			return nil, err
		}
		badge := 9
		if n < badge+2 {
			badge = n - 2
			if badge%2 == 0 {
				badge--
			}
		}
		start := (n - badge) / 2
		fmt.Fprintf(&b, `<rect x="%d" y="%d" width="%d" height="%d" fill="#fff"/>`, start, start, badge, badge)
		fmt.Fprintf(&b, `<image x="%d" y="%d" width="%d" height="%d" href="data:image/png;base64,%s"/>`, start, start, badge, badge, mark)
	}
	b.WriteString(`</svg>`)
	return []byte(b.String()), nil
}

func logoMark() (string, error) {
	markOnce.Do(func() {
		var buf bytes.Buffer
		if err := png.Encode(&buf, clubLogo(512)); err != nil {
			markErr = err
			return
		}
		markData = base64.StdEncoding.EncodeToString(buf.Bytes())
	})
	return markData, markErr
}
