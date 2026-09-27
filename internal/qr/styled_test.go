package qr

import (
	"bytes"
	"image/png"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

const styledContent = "https://skyl.app/denemene?utm_source=qr"

func TestStyledPNGDrawsRoundedEyesWithAHole(t *testing.T) {
	t.Parallel()
	// 37 modules plus the quiet zone is 45, so 450 px is 10 px a module.
	raw, err := StyledPNG(styledContent, 450, true)
	if err != nil {
		t.Fatal(err)
	}
	img, err := png.Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if b := img.Bounds(); b.Dx() != 450 || b.Dy() != 450 {
		t.Fatalf("bounds %v", b)
	}
	dark := func(x, y int) bool {
		r, _, _, _ := img.At(x, y).RGBA()
		return r < 0x4000
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
		if got := dark(c.x, c.y); got != c.want {
			t.Errorf("%s at (%d,%d): dark=%v", c.name, c.x, c.y, got)
		}
	}
}

func TestStyledPNGChangesWithTheMark(t *testing.T) {
	t.Parallel()
	plain, err := StyledPNG(styledContent, 256, false)
	if err != nil {
		t.Fatal(err)
	}
	withMark, err := StyledPNG(styledContent, 256, true)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(plain, withMark) {
		t.Fatal("the mark should change the png")
	}
}

func TestStyledSVGKeepsTheMarkInProportion(t *testing.T) {
	t.Parallel()
	svg, err := StyledSVG(styledContent, true)
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

	plain, err := StyledSVG(styledContent, false)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(plain), "<image") {
		t.Fatal("a code without the logo must not embed the mark")
	}
}
