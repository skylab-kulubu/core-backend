package qr

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/draw"
	"image/png"
	"math"
	"strconv"
	"strings"
	"sync"

	qrcode "github.com/skip2/go-qrcode"
	"golang.org/x/image/vector"
)

// The short-link style: soft square modules, each finder eye drawn as one
// rounded shape, and a rounded clearing that keeps the club mark in its own
// proportions. SVG and PNG are drawn from the same layout.
const (
	// quietZone is narrower than the four modules the QR standard asks for:
	// phones read 2.5 without trouble, and a wider margin only made the code
	// look small. The corner code sits in the corner square of this margin.
	quietZone     = 2.5
	clearModules  = 11
	clearShare    = 0.3
	clearRounding = 0.24
	markPadding   = 1.3
	// Below this many pixels a module, rounding only blurs module edges into
	// grey, so a small PNG falls back to plain squares.
	minStyledPixels = 4
	// kappa places cubic control points so a quarter curve follows a circle.
	kappa = 0.5523
)

type moduleShape struct {
	inset, radius, eye, eyeDot float64
}

var (
	softModules   = moduleShape{inset: 0.06, radius: 0.26, eye: 1.7, eyeDot: 0.75}
	squareModules = moduleShape{}
)

type roundRect struct {
	x, y, w, h, r float64
	// hole is cut out of the shape it sits in (the ring inside an eye).
	hole bool
}

type styledLayout struct {
	side  float64
	rects []roundRect
	mark  *roundRect
}

func layoutStyled(content string, logo bool, shape moduleShape) (styledLayout, error) {
	// The mark covers modules, so a code carrying it needs the highest
	// error correction to stay readable.
	level := qrcode.Medium
	if logo {
		level = qrcode.Highest
	}
	code, err := qrcode.New(content, level)
	if err != nil {
		return styledLayout{}, err
	}
	code.DisableBorder = true
	bitmap := code.Bitmap()
	n := len(bitmap)
	q := quietZone
	out := styledLayout{side: float64(n) + 2*quietZone}

	clear := 0.0
	if logo {
		clear = math.Min(clearModules, float64(n)*clearShare)
	}
	clearAt := (float64(n) - clear) / 2
	inEye := func(x, y int) bool {
		return (x < 7 && y < 7) || (x >= n-7 && y < 7) || (x < 7 && y >= n-7)
	}
	for y, row := range bitmap {
		for x, dark := range row {
			if !dark || inEye(x, y) {
				continue
			}
			if clear > 0 && insideRoundRect(float64(x)+0.5, float64(y)+0.5, roundRect{x: clearAt, y: clearAt, w: clear, h: clear, r: clear * clearRounding}) {
				continue
			}
			size := 1 - 2*shape.inset
			out.rects = append(out.rects, roundRect{x: q + float64(x) + shape.inset, y: q + float64(y) + shape.inset, w: size, h: size, r: shape.radius})
		}
	}
	for _, at := range [][2]int{{0, 0}, {n - 7, 0}, {0, n - 7}} {
		ox, oy := q+float64(at[0]), q+float64(at[1])
		out.rects = append(out.rects,
			roundRect{x: ox, y: oy, w: 7, h: 7, r: shape.eye},
			roundRect{x: ox + 1, y: oy + 1, w: 5, h: 5, r: math.Max(shape.eye-1, 0), hole: true},
			roundRect{x: ox + 2, y: oy + 2, w: 3, h: 3, r: shape.eyeDot},
		)
	}
	if clear > 0 {
		w := clear - 2*markPadding
		h := w * markAspect()
		center := q + float64(n)/2
		out.mark = &roundRect{x: center - w/2, y: center - h/2, w: w, h: h}
	}
	return out, nil
}

func insideRoundRect(px, py float64, r roundRect) bool {
	if px < r.x || px > r.x+r.w || py < r.y || py > r.y+r.h {
		return false
	}
	nx := math.Min(math.Max(px, r.x+r.r), r.x+r.w-r.r)
	ny := math.Min(math.Max(py, r.y+r.r), r.y+r.h-r.r)
	return (px-nx)*(px-nx)+(py-ny)*(py-ny) <= r.r*r.r
}

// StyledSVG draws the code as vector shapes so a poster stays sharp at any
// print size. The mark is embedded as an image because it only exists as a
// PNG. A non-empty corner is printed small in the bottom-right corner of the
// margin, so a printed code shows which channel it counts for.
func StyledSVG(content string, logo bool, corner string) ([]byte, error) {
	layout, err := layoutStyled(content, logo, softModules)
	if err != nil {
		return nil, err
	}
	side := num(layout.side)
	var b strings.Builder
	b.WriteString(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 ` + side + " " + side + `">`)
	b.WriteString(`<rect width="` + side + `" height="` + side + `" fill="#fff"/>`)
	b.WriteString(`<path fill="#000" fill-rule="evenodd" d="`)
	for _, r := range layout.rects {
		writeRoundRect(&b, r)
	}
	b.WriteString(`"/>`)
	if corner != "" {
		segments, err := cornerOutline(corner, layout.side)
		if err != nil {
			return nil, err
		}
		b.WriteString(`<path fill="#000" d="`)
		writeOutline(&b, segments)
		b.WriteString(`"/>`)
	}
	if layout.mark != nil {
		mark, err := markPNG()
		if err != nil {
			return nil, err
		}
		m := layout.mark
		b.WriteString(`<image x="` + num(m.x) + `" y="` + num(m.y) + `" width="` + num(m.w) + `" height="` + num(m.h) + `" href="data:image/png;base64,` + mark + `"/>`)
	}
	b.WriteString(`</svg>`)
	return []byte(b.String()), nil
}

func writeRoundRect(b *strings.Builder, r roundRect) {
	x, y, w, h, rad := r.x, r.y, r.w, r.h, r.r
	arc := func(ex, ey float64) {
		b.WriteString("A" + num(rad) + " " + num(rad) + " 0 0 1 " + num(ex) + " " + num(ey))
	}
	b.WriteString("M" + num(x+rad) + " " + num(y) + "H" + num(x+w-rad))
	arc(x+w, y+rad)
	b.WriteString("V" + num(y+h-rad))
	arc(x+w-rad, y+h)
	b.WriteString("H" + num(x+rad))
	arc(x, y+h-rad)
	b.WriteString("V" + num(y+rad))
	arc(x+rad, y)
	b.WriteString("Z")
}

func num(v float64) string {
	return strconv.FormatFloat(math.Round(v*1000)/1000, 'f', -1, 64)
}

// StyledPNG rasterizes the same layout as StyledSVG, anti-aliased, on a
// size×size white square.
func StyledPNG(content string, size int, logo bool, corner string) ([]byte, error) {
	if size < 64 || size > 1024 {
		size = DefaultSize
	}
	layout, err := layoutStyled(content, logo, softModules)
	if err != nil {
		return nil, err
	}
	// Whole pixels per module keep module edges crisp, which small codes need
	// to scan; the pixels left over widen the white margin.
	scale := math.Floor(float64(size) / layout.side)
	if scale < 1 {
		scale = float64(size) / layout.side
	}
	if scale < minStyledPixels {
		if layout, err = layoutStyled(content, logo, squareModules); err != nil {
			return nil, err
		}
	}
	// The symbol, not the margin, is put on whole pixels: a 2.5-module margin
	// would otherwise start every module mid-pixel whenever scale is odd.
	offset := math.Floor((float64(size)-scale*(layout.side-2*quietZone))/2) - quietZone*scale
	raster := vector.NewRasterizer(size, size)
	for _, r := range layout.rects {
		addRoundRect(raster, r, scale, offset)
	}
	// Below minStyledPixels the code would be a few unreadable pixels, so a
	// small PNG leaves it out.
	if corner != "" && scale >= minStyledPixels {
		segments, err := cornerOutline(corner, layout.side)
		if err != nil {
			return nil, err
		}
		addOutline(raster, segments, scale, offset)
	}
	dst := image.NewRGBA(image.Rect(0, 0, size, size))
	draw.Draw(dst, dst.Bounds(), image.White, image.Point{}, draw.Src)
	raster.Draw(dst, dst.Bounds(), image.Black, image.Point{})
	if layout.mark != nil {
		m := layout.mark
		px := func(v float64) int { return int(math.Round(offset + v*scale)) }
		box := image.Rect(px(m.x), px(m.y), px(m.x+m.w), px(m.y+m.h))
		draw.Draw(dst, box, blackMark(box.Dx(), box.Dy()), image.Point{}, draw.Over)
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, dst); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// addRoundRect traces r clockwise, or counter-clockwise for a hole: the
// rasterizer adds up winding, so only the reversed direction cuts the ring
// out of an eye.
func addRoundRect(z *vector.Rasterizer, r roundRect, scale, offset float64) {
	x, y, w, h, rad := offset+r.x*scale, offset+r.y*scale, r.w*scale, r.h*scale, r.r*scale
	k := rad * kappa
	type point struct{ x, y float64 }
	type curve struct{ c1, c2, end point }
	start := point{x + rad, y}
	segments := []curve{
		{point{x + w - rad, y}, point{x + w - rad, y}, point{x + w - rad, y}},
		{point{x + w - rad + k, y}, point{x + w, y + rad - k}, point{x + w, y + rad}},
		{point{x + w, y + h - rad}, point{x + w, y + h - rad}, point{x + w, y + h - rad}},
		{point{x + w, y + h - rad + k}, point{x + w - rad + k, y + h}, point{x + w - rad, y + h}},
		{point{x + rad, y + h}, point{x + rad, y + h}, point{x + rad, y + h}},
		{point{x + rad - k, y + h}, point{x, y + h - rad + k}, point{x, y + h - rad}},
		{point{x, y + rad}, point{x, y + rad}, point{x, y + rad}},
		{point{x, y + rad - k}, point{x + rad - k, y}, start},
	}
	f := func(v float64) float32 { return float32(v) }
	z.MoveTo(f(start.x), f(start.y))
	if !r.hole {
		for _, s := range segments {
			z.CubeTo(f(s.c1.x), f(s.c1.y), f(s.c2.x), f(s.c2.y), f(s.end.x), f(s.end.y))
		}
	} else {
		for i := len(segments) - 1; i >= 0; i-- {
			from := start
			if i > 0 {
				from = segments[i-1].end
			}
			s := segments[i]
			z.CubeTo(f(s.c2.x), f(s.c2.y), f(s.c1.x), f(s.c1.y), f(from.x), f(from.y))
		}
	}
	z.ClosePath()
}

var (
	markOnce sync.Once
	markData string
	markErr  error
)

func markPNG() (string, error) {
	markOnce.Do(func() {
		const width = 512
		var buf bytes.Buffer
		if err := png.Encode(&buf, blackMark(width, int(math.Round(width*markAspect())))); err != nil {
			markErr = err
			return
		}
		markData = base64.StdEncoding.EncodeToString(buf.Bytes())
	})
	return markData, markErr
}
