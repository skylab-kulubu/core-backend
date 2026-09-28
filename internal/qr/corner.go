package qr

import (
	"strings"
	"sync"

	"golang.org/x/image/font"
	"golang.org/x/image/font/gofont/gomonobold"
	"golang.org/x/image/font/sfnt"
	"golang.org/x/image/math/fixed"
	"golang.org/x/image/vector"
)

// The corner code is set in Go Mono Bold and drawn as outlines, so the SVG
// and the PNG show the same letters without needing the font where they are
// opened. Sizes are in modules.
const (
	cornerEm       = 1.3
	cornerInset    = 0.5
	cornerBaseline = 0.55
	cornerUnits    = 1024
)

type outlineSegment struct {
	op  sfnt.SegmentOp
	pts [3][2]float64
}

var (
	cornerFontOnce sync.Once
	cornerFont     *sfnt.Font
	cornerFontErr  error
)

// cornerOutline lays text out right-aligned in the bottom-right corner of a
// code side modules wide.
func cornerOutline(text string, side float64) ([]outlineSegment, error) {
	cornerFontOnce.Do(func() { cornerFont, cornerFontErr = sfnt.Parse(gomonobold.TTF) })
	if cornerFontErr != nil {
		return nil, cornerFontErr
	}
	var buf sfnt.Buffer
	ppem := fixed.I(cornerUnits)
	scale := cornerEm / cornerUnits
	type glyph struct {
		segments sfnt.Segments
		advance  float64
	}
	var glyphs []glyph
	width := 0.0
	for _, r := range text {
		index, err := cornerFont.GlyphIndex(&buf, r)
		if err != nil {
			return nil, err
		}
		segments, err := cornerFont.LoadGlyph(&buf, index, ppem, nil)
		if err != nil {
			return nil, err
		}
		advance, err := cornerFont.GlyphAdvance(&buf, index, ppem, font.HintingNone)
		if err != nil {
			return nil, err
		}
		g := glyph{segments: append(sfnt.Segments(nil), segments...), advance: float64(advance) / 64 * scale}
		glyphs = append(glyphs, g)
		width += g.advance
	}
	pen := side - cornerInset - width
	baseline := side - cornerBaseline
	var out []outlineSegment
	for _, g := range glyphs {
		for _, s := range g.segments {
			seg := outlineSegment{op: s.Op}
			for i, p := range s.Args {
				seg.pts[i] = [2]float64{pen + float64(p.X)/64*scale, baseline + float64(p.Y)/64*scale}
			}
			out = append(out, seg)
		}
		pen += g.advance
	}
	return out, nil
}

func writeOutline(b *strings.Builder, segments []outlineSegment) {
	point := func(p [2]float64) string { return num(p[0]) + " " + num(p[1]) }
	open := false
	for _, s := range segments {
		switch s.op {
		case sfnt.SegmentOpMoveTo:
			if open {
				b.WriteString("Z")
			}
			b.WriteString("M" + point(s.pts[0]))
			open = true
		case sfnt.SegmentOpLineTo:
			b.WriteString("L" + point(s.pts[0]))
		case sfnt.SegmentOpQuadTo:
			b.WriteString("Q" + point(s.pts[0]) + " " + point(s.pts[1]))
		case sfnt.SegmentOpCubeTo:
			b.WriteString("C" + point(s.pts[0]) + " " + point(s.pts[1]) + " " + point(s.pts[2]))
		}
	}
	if open {
		b.WriteString("Z")
	}
}

func addOutline(z *vector.Rasterizer, segments []outlineSegment, scale, offset float64) {
	f := func(v float64) float32 { return float32(offset + v*scale) }
	open := false
	for _, s := range segments {
		p := s.pts
		switch s.op {
		case sfnt.SegmentOpMoveTo:
			if open {
				z.ClosePath()
			}
			z.MoveTo(f(p[0][0]), f(p[0][1]))
			open = true
		case sfnt.SegmentOpLineTo:
			z.LineTo(f(p[0][0]), f(p[0][1]))
		case sfnt.SegmentOpQuadTo:
			z.QuadTo(f(p[0][0]), f(p[0][1]), f(p[1][0]), f(p[1][1]))
		case sfnt.SegmentOpCubeTo:
			z.CubeTo(f(p[0][0]), f(p[0][1]), f(p[1][0]), f(p[1][1]), f(p[2][0]), f(p[2][1]))
		}
	}
	if open {
		z.ClosePath()
	}
}
