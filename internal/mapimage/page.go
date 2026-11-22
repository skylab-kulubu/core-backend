package mapimage

import (
	"strings"

	"github.com/skylab-kulubu/core-backend/internal/brandfont"
)

// PageHTML is the page Gotenberg takes the map's picture of: the SVG alone,
// at the picture's size, with the embedded Manrope for its names. It holds
// nothing to fetch.
func PageHTML(svg string) string {
	var b strings.Builder
	b.WriteString(`<!doctype html><html lang="tr"><head><meta charset="utf-8"><style>`)
	b.WriteString(brandfont.FaceCSS())
	b.WriteString(`html,body{margin:0;padding:0;background:#FFFFFF}svg{display:block}</style></head><body>`)
	b.WriteString(svg)
	b.WriteString(`</body></html>`)
	return b.String()
}
