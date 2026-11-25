package certificate

import "math"

// The ways a background is placed on the page (BackgroundPlacement.Fit).
// contain, cover, fill and none mean what CSS object-fit means; fitWidth
// and fitHeight scale the background to the page's width or height.
const (
	FitContain   = "contain"
	FitCover     = "cover"
	FitFill      = "fill"
	FitWidth     = "fitWidth"
	FitHeight    = "fitHeight"
	FitNone      = "none"
	minBackScale = 0.1
	maxBackScale = 10
)

// BackgroundPlacement places a layout's background on its page. X and Y
// are CSS object-position fractions (0 left/top, 0.5 centre, 1
// right/bottom); Scale multiplies the scale the fit finds and is ignored
// by fill.
type BackgroundPlacement struct {
	Fit   string  `json:"fit"`
	X     float64 `json:"x"`
	Y     float64 `json:"y"`
	Scale float64 `json:"scale"`
}

// Rect is a box on the page in layout px. It may reach past the page; the
// page clips it.
type Rect struct {
	Left   float64 `json:"left"`
	Top    float64 `json:"top"`
	Width  float64 `json:"width"`
	Height float64 `json:"height"`
}

// LegacyImagePlacement is how an image background of a layout without a
// placement is printed: the page is covered and the image centred (CSS
// background-size:cover; background-position:center).
var LegacyImagePlacement = BackgroundPlacement{Fit: FitCover, X: 0.5, Y: 0.5, Scale: 1}

// LegacyPDFPlacement is how a PDF background of a layout without a
// placement is printed. Gotenberg 8.37 lays it under the page with pdfcpu
// v0.15.0's watermark "scale:1" (relative): pdfcpu scales a source wider
// than tall (or square) to the page's width and a taller one to the page's
// height, and centres it (pkg/pdfcpu/model/watermark.go CalcBoundingBox
// and LowerLeftCorner, anchor "c"). srcW and srcH are the PDF page's
// visible box.
func LegacyPDFPlacement(srcW, srcH float64) BackgroundPlacement {
	if srcW >= srcH {
		return BackgroundPlacement{Fit: FitWidth, X: 0.5, Y: 0.5, Scale: 1}
	}
	return BackgroundPlacement{Fit: FitHeight, X: 0.5, Y: 0.5, Scale: 1}
}

// PlaceBackground is the box a background of natural size srcW×srcH takes
// on a pageW×pageH page. The panel's editor computes the same box
// (core-frontend src/lib/certificate-placement.ts); both are tested with
// testdata/background-placement.json.
func PlaceBackground(pageW, pageH, srcW, srcH float64, p BackgroundPlacement) Rect {
	if p.Fit == FitFill {
		return Rect{Width: pageW, Height: pageH}
	}
	var k float64
	switch p.Fit {
	case FitContain:
		k = math.Min(pageW/srcW, pageH/srcH)
	case FitCover:
		k = math.Max(pageW/srcW, pageH/srcH)
	case FitWidth:
		k = pageW / srcW
	case FitHeight:
		k = pageH / srcH
	default: // FitNone
		k = 1
	}
	k *= p.Scale
	w, h := srcW*k, srcH*k
	return Rect{Left: (pageW - w) * p.X, Top: (pageH - h) * p.Y, Width: w, Height: h}
}

func validPlacement(p BackgroundPlacement) bool {
	switch p.Fit {
	case FitContain, FitCover, FitFill, FitWidth, FitHeight, FitNone:
	default:
		return false
	}
	return finite(p.X, p.Y, p.Scale) &&
		p.X >= 0 && p.X <= 1 && p.Y >= 0 && p.Y <= 1 &&
		p.Scale >= minBackScale && p.Scale <= maxBackScale
}
