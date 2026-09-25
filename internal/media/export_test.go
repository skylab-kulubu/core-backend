package media

import "time"

// LimitSVGDrawingTo sets how long drawing one SVG may take, for a test; the
// returned restore puts back the limit it replaced.
func LimitSVGDrawingTo(limit time.Duration) (restore func()) {
	previous := svgDrawingLimit
	svgDrawingLimit = limit
	return func() { svgDrawingLimit = previous }
}
