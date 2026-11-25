package certificate

import (
	"bytes"
	"encoding/xml"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"strconv"
	"strings"

	"github.com/skylab-kulubu/core-backend/internal/media"
	_ "golang.org/x/image/webp"
)

// backgroundSize is an image background's natural size in CSS px, as the
// browser that prints the page sees it: a JPEG turned by its EXIF
// orientation counts turned, and an SVG has the size its width and height
// (or its viewBox) give.
func backgroundSize(contentType string, data []byte) (float64, float64, error) {
	var w, h float64
	if strings.EqualFold(strings.TrimSpace(strings.Split(contentType, ";")[0]), "image/svg+xml") {
		w, h = svgSize(data)
	} else if config, format, err := image.DecodeConfig(bytes.NewReader(data)); err == nil {
		w, h = float64(config.Width), float64(config.Height)
		if o := media.JPEGOrientation(data); format == "jpeg" && o >= 5 && o <= 8 {
			w, h = h, w
		}
	}
	// A size under minBackgroundSide (an SVG of width="0.0000001") would
	// scale the background past any page.
	if !finite(w, h) || w < minBackgroundSide || h < minBackgroundSide {
		return 0, 0, invalid(CodeBackgroundSizeUnknown)
	}
	return w, h, nil
}

// minBackgroundSide is the smallest natural side, in px, a placed
// background may have.
const minBackgroundSide = 1e-3

// svgSize reads the root svg element's width and height in an absolute
// unit (px or unitless, pt, pc, mm, cm, in); when one is missing or in a
// relative unit (%, em) the viewBox gives it, keeping the viewBox's ratio.
func svgSize(data []byte) (float64, float64) {
	decoder := xml.NewDecoder(bytes.NewReader(data))
	for {
		token, err := decoder.Token()
		if err != nil {
			return 0, 0
		}
		start, ok := token.(xml.StartElement)
		if !ok {
			continue
		}
		if start.Name.Local != "svg" {
			return 0, 0
		}
		var w, h, vw, vh float64
		for _, attr := range start.Attr {
			switch attr.Name.Local {
			case "width":
				w = svgLength(attr.Value)
			case "height":
				h = svgLength(attr.Value)
			case "viewBox":
				fields := strings.FieldsFunc(attr.Value, func(r rune) bool { return r == ' ' || r == ',' || r == '\t' || r == '\n' || r == '\r' })
				if len(fields) == 4 {
					vw, _ = strconv.ParseFloat(fields[2], 64)
					vh, _ = strconv.ParseFloat(fields[3], 64)
				}
			}
		}
		switch {
		case w > 0 && h > 0:
			return w, h
		case !(vw > 0 && vh > 0):
			return 0, 0
		case w > 0:
			return w, w * vh / vw
		case h > 0:
			return h * vw / vh, h
		default:
			return vw, vh
		}
	}
}

// svgUnits are the absolute CSS units in px (96 per inch).
var svgUnits = map[string]float64{"px": 1, "pt": 96.0 / 72, "pc": 16, "mm": 96 / 25.4, "cm": 96 / 2.54, "in": 96}

// svgLength is an SVG width or height in px; 0 when it is missing, not a
// number or in a relative unit.
func svgLength(value string) float64 {
	value = strings.ToLower(strings.TrimSpace(value))
	factor := 1.0
	if len(value) > 2 {
		if f, ok := svgUnits[value[len(value)-2:]]; ok {
			factor, value = f, strings.TrimSpace(value[:len(value)-2])
		}
	}
	n, err := strconv.ParseFloat(value, 64)
	if err != nil || !finite(n) {
		return 0
	}
	return n * factor
}
