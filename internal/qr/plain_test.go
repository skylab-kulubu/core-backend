package qr

import (
	"regexp"
	"strconv"
	"strings"
	"testing"

	qrcode "github.com/skip2/go-qrcode"
)

func TestPlainSVGDrawsEveryDarkModuleOnce(t *testing.T) {
	t.Parallel()
	const content = "https://api.yildizskylab.com/v1/sessions/11111111-1111-1111-1111-111111111111?dq=v1.t3kq8g.AbCdEfGh.0123456789abcdef"
	svg, err := PlainSVG(content)
	if err != nil {
		t.Fatal(err)
	}
	code, err := qrcode.New(content, qrcode.Medium)
	if err != nil {
		t.Fatal(err)
	}
	code.DisableBorder = true
	want := code.Bitmap()
	n := len(want)
	if !strings.Contains(string(svg), `viewBox="0 0 `+strconv.Itoa(n+8)+" "+strconv.Itoa(n+8)+`"`) {
		t.Fatalf("viewBox: %.200s", svg)
	}
	got := make([][]bool, n)
	for i := range got {
		got[i] = make([]bool, n)
	}
	runs := regexp.MustCompile(`M(\d+) (\d+)h(\d+)v1h-(\d+)z`).FindAllStringSubmatch(string(svg), -1)
	for _, r := range runs {
		x, _ := strconv.Atoi(r[1])
		y, _ := strconv.Atoi(r[2])
		w, _ := strconv.Atoi(r[3])
		for i := range w {
			if got[y-4][x-4+i] {
				t.Fatalf("module %d,%d drawn twice", x-4+i, y-4)
			}
			got[y-4][x-4+i] = true
		}
	}
	for y := range n {
		for x := range n {
			if got[y][x] != want[y][x] {
				t.Fatalf("module %d,%d: got %v want %v", x, y, got[y][x], want[y][x])
			}
		}
	}
}
