package qr

import (
	"bytes"
	"image"
	"image/png"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

const styledContent = "https://skyl.app/denemene?utm_source=qr"

func decodePNG(t *testing.T, raw []byte) image.Image {
	t.Helper()
	img, err := png.Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	return img
}

func darkAt(img image.Image, x, y int) bool {
	r, _, _, _ := img.At(x, y).RGBA()
	return r < 0x4000
}

func TestStyledPNGDrawsRoundedEyesWithAHole(t *testing.T) {
	t.Parallel()
	// 37 modules plus the quiet zone is 42, so 450 px is 10 px a module with
	// 15 px left over on each side: module x starts at 40 + 10x.
	img := decodePNG(t, mustPNG(t, styledContent, 450, true, ""))
	if b := img.Bounds(); b.Dx() != 450 || b.Dy() != 450 {
		t.Fatalf("bounds %v", b)
	}
	for _, c := range []struct {
		name string
		x, y int
		want bool
	}{
		{"quiet zone", 20, 20, false},
		{"rounded eye corner", 41, 41, false},
		{"eye ring", 45, 75, true},
		{"ring hole", 55, 75, false},
		{"eye dot", 75, 75, true},
	} {
		if got := darkAt(img, c.x, c.y); got != c.want {
			t.Errorf("%s at (%d,%d): dark=%v", c.name, c.x, c.y, got)
		}
	}
}

func TestStyledPNGPrintsTheCornerCodeInTheMarginCorner(t *testing.T) {
	t.Parallel()
	// Same 37-module code as above: the symbol ends at 410 px, so the square
	// from 410 to 435 is the corner of the margin.
	content := styledContent
	countCorner := func(img image.Image) int {
		n := 0
		for y := 410; y < 435; y++ {
			for x := 410; x < 435; x++ {
				if darkAt(img, x, y) {
					n++
				}
			}
		}
		return n
	}
	if n := countCorner(decodePNG(t, mustPNG(t, content, 450, true, "ig"))); n < 20 {
		t.Fatalf("corner code left %d dark pixels", n)
	}
	if n := countCorner(decodePNG(t, mustPNG(t, content, 450, true, ""))); n != 0 {
		t.Fatalf("no corner code but %d dark pixels in the corner", n)
	}
}

func TestStyledPNGChangesWithTheMark(t *testing.T) {
	t.Parallel()
	if bytes.Equal(mustPNG(t, styledContent, 256, false, ""), mustPNG(t, styledContent, 256, true, "")) {
		t.Fatal("the mark should change the png")
	}
}

func TestStyledSVGKeepsTheMarkInProportion(t *testing.T) {
	t.Parallel()
	svg, err := StyledSVG(styledContent, true, "")
	if err != nil {
		t.Fatal(err)
	}
	body := string(svg)
	if !strings.HasPrefix(body, "<svg") || !strings.Contains(body, `fill-rule="evenodd"`) {
		t.Fatalf("svg %.160s", body)
	}
	match := regexp.MustCompile(`<image x="[^"]+" y="[^"]+" width="([^"]+)" height="([^"]+)"`).FindStringSubmatch(body)
	if match == nil {
		t.Fatal("no mark image")
	}
	w, _ := strconv.ParseFloat(match[1], 64)
	h, _ := strconv.ParseFloat(match[2], 64)
	if got, want := h/w, markAspect(); got < want-0.01 || got > want+0.01 {
		t.Fatalf("mark ratio %.3f, want %.3f", got, want)
	}
	if strings.Count(body, "<path") != 1 {
		t.Fatal("a code without a corner code draws one path")
	}

	plain, err := StyledSVG(styledContent, false, "")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(plain), "<image") {
		t.Fatal("a code without the logo must not embed the mark")
	}
}

func TestStyledSVGDrawsTheCornerCodeAsOutlines(t *testing.T) {
	t.Parallel()
	svg, err := StyledSVG(styledContent, true, "yt")
	if err != nil {
		t.Fatal(err)
	}
	body := string(svg)
	if strings.Count(body, "<path") != 2 || strings.Contains(body, "<text") {
		t.Fatalf("corner code %.200s", body[strings.LastIndex(body, "<path"):])
	}
}

func mustPNG(t *testing.T, content string, size int, logo bool, corner string) []byte {
	t.Helper()
	raw, err := StyledPNG(content, size, logo, corner)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
