package media

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"image"
	"io"
	"math"
	"time"

	"github.com/fyne-io/oksvg"
	"github.com/srwiley/rasterx"
	"golang.org/x/image/math/fixed"
)

// Limits on the SVG core rasterizes, so that an uploaded SVG costs a
// bounded time and memory: the bytes, the elements (after a <use> copies
// its definitions), how deep they nest, and how long drawing may take.
const (
	maxSVGBytes    = 1 << 20
	maxSVGElements = 10_000
	maxSVGDepth    = 64
	// svgRasterSide is the longer side, in pixels, of the PNG an SVG
	// becomes: the page size. Drawing costs the canvas once per shape, so a
	// larger canvas would put detailed logos over the time limit.
	svgRasterSide = 1200
)

var (
	// errSVGTooLarge refuses an SVG above maxSVGBytes.
	errSVGTooLarge = errors.New("media: SVG too large to rasterize")
	// errSVGNotDrawn refuses an SVG core does not rasterize: not XML, a
	// document type, too many or too deep elements, a <use> inside <defs>
	// (which could refer to itself), no size, or too slow to draw.
	errSVGNotDrawn = errors.New("media: SVG core does not rasterize")
	// errSVGDrawingTooSlow stops a drawing that has run past its limit.
	errSVGDrawingTooSlow = errors.New("media: SVG drawing ran past its time limit")
)

// rasterizeSVG draws an SVG into a PNG whose longer side is svgRasterSide
// (or the purpose's maximum dimension, when smaller) and makes the
// purpose's sizes of it. Nothing of the SVG itself is kept: scripts, links
// and the markup go with it. The drawing is pure Go (oksvg, rasterx), in
// the request, within the limits above and at most limit long (the decode
// budget's SVG drawing limit); dashes are drawn solid. The caller holds the
// decode budget's SVG slot.
func rasterizeSVG(ctx context.Context, data []byte, handling ImageHandling, limit time.Duration) (reencodedImage, error) {
	if len(data) > maxSVGBytes {
		return reencodedImage{}, errSVGTooLarge
	}
	if err := checkSVGStructure(data); err != nil {
		return reencodedImage{}, err
	}
	drawing, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	img, err := drawSVG(drawing, data, min(svgRasterSide, handling.maxDimension()))
	if err != nil {
		return reencodedImage{}, err
	}
	return finishImage(img, "image/png", handling.Sizes)
}

// checkSVGStructure reads the SVG as XML before anything draws it and
// refuses what would make drawing unbounded.
func checkSVGStructure(data []byte) error {
	decoder := xml.NewDecoder(bytes.NewReader(data))
	var elements, depth, defsDepth, uses, defined int
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return errSVGNotDrawn
		}
		switch t := token.(type) {
		case xml.Directive:
			// A document type could declare entities.
			return errSVGNotDrawn
		case xml.StartElement:
			elements++
			depth++
			if elements > maxSVGElements || depth > maxSVGDepth {
				return errSVGNotDrawn
			}
			inDefs := defsDepth > 0
			switch {
			case t.Name.Local == "use" && inDefs:
				return errSVGNotDrawn
			case t.Name.Local == "use":
				uses++
			case t.Name.Local == "defs" && !inDefs:
				defsDepth = depth
			case inDefs:
				defined++
			}
		case xml.EndElement:
			if depth == defsDepth {
				defsDepth = 0
			}
			depth--
		}
	}
	// Each <use> draws the definitions it names again: at most all of them.
	if elements == 0 || elements+uses*defined > maxSVGElements {
		return errSVGNotDrawn
	}
	return nil
}

// drawSVG draws the SVG on a transparent canvas whose longer side is side,
// until ctx ends: checked between shapes, and on every line a shape is
// flattened into. A single call into the rasterizer between two checks can
// still run on past the deadline.
func drawSVG(ctx context.Context, data []byte, side int) (img *image.RGBA, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			// The time limit, or a fault in the parser on a hostile file:
			// either way the SVG is refused, never core stopped.
			img, err = nil, errSVGNotDrawn
		}
	}()
	icon, err := oksvg.ReadIconStream(bytes.NewReader(data))
	if err != nil {
		return nil, errSVGNotDrawn
	}
	box := icon.ViewBox
	if !finitePositive(box.W) || !finitePositive(box.H) {
		return nil, errSVGNotDrawn
	}
	scale := float64(side) / math.Max(box.W, box.H)
	w := max(1, int(math.Round(box.W*scale)))
	h := max(1, int(math.Round(box.H*scale)))
	icon.SetTarget(0, 0, float64(w), float64(h))
	img = image.NewRGBA(image.Rect(0, 0, w, h))
	scanner := &deadlineScanner{ScannerGV: rasterx.NewScannerGV(w, h, img, img.Bounds()), done: ctx.Done()}
	raster := rasterx.NewDasher(w, h, scanner)
	for i := range icon.SVGPaths {
		if ctx.Err() != nil {
			return nil, errSVGNotDrawn
		}
		path := &icon.SVGPaths[i]
		path.Dash = nil
		path.DrawTransformed(raster, 1, icon.Transform)
	}
	return img, nil
}

func finitePositive(v float64) bool {
	return v > 0 && !math.IsInf(v, 0) && !math.IsNaN(v)
}

// deadlineScanner is the rasterizer's scanner, stopping a drawing whose
// context ended. Every line a shape is flattened into passes here in
// github.com/srwiley/rasterx (an untagged 2022 commit), so one huge shape
// cannot run on unchecked between shapes; the check relies on that
// internal behaviour, and the checks between shapes do not.
type deadlineScanner struct {
	*rasterx.ScannerGV
	done <-chan struct{}
}

func (s *deadlineScanner) Line(b fixed.Point26_6) {
	select {
	case <-s.done:
		panic(errSVGDrawingTooSlow)
	default:
	}
	s.ScannerGV.Line(b)
}
