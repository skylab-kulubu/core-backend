// Package brandfont is the club's typeface for the documents core draws:
// Manrope, the club site's (SIL Open Font License 1.1, fonts/OFL.txt),
// embedded so that the renderer (Gotenberg's Chromium) reaches nothing
// outside the page. The ticket PDF and the event map use it.
package brandfont

import (
	"embed"
	"encoding/base64"
	"strconv"
	"strings"
)

// Family is the CSS font family the rules of FaceCSS name.
const Family = "Manrope"

// The static instances of five weights, each in its Latin and Latin
// Extended subsets (Turkish needs both), as Fontsource 5.3.0 publishes
// them. Static fonts stay text in a PDF; a variable font would be drawn as
// Type 3 glyphs.
//
//go:embed fonts/*.woff2
var files embed.FS

// Weights are the weights embedded.
var Weights = []int{400, 500, 600, 700, 800}

// ranges are the subsets' unicode ranges, as Fontsource's CSS gives them.
var ranges = map[string]string{
	"latin":     "U+0000-00FF,U+0131,U+0152-0153,U+02BB-02BC,U+02C6,U+02DA,U+02DC,U+0304,U+0308,U+0329,U+2000-206F,U+20AC,U+2122,U+2191,U+2193,U+2212,U+2215,U+FEFF,U+FFFD",
	"latin-ext": "U+0100-02BA,U+02BD-02C5,U+02C7-02CC,U+02CE-02D7,U+02DD-02FF,U+0304,U+0308,U+0329,U+1D00-1DBF,U+1E00-1E9F,U+1EF2-1EFF,U+2020,U+20A0-20AB,U+20AD-20C0,U+2113,U+2C60-2C7F,U+A720-A7FF",
}

// FaceCSS is the @font-face rules of the embedded Manrope, each file a data
// URL.
func FaceCSS() string {
	var b strings.Builder
	for _, weight := range Weights {
		for _, subset := range []string{"latin-ext", "latin"} {
			name := "fonts/manrope-" + subset + "-" + strconv.Itoa(weight) + "-normal.woff2"
			data, err := files.ReadFile(name)
			if err != nil {
				panic("brandfont: embedded font missing: " + name)
			}
			b.WriteString("@font-face{font-family:'" + Family + "';font-style:normal;font-display:block;font-weight:")
			b.WriteString(strconv.Itoa(weight))
			b.WriteString(";src:url(data:font/woff2;base64,")
			b.WriteString(base64.StdEncoding.EncodeToString(data))
			b.WriteString(") format('woff2');unicode-range:")
			b.WriteString(ranges[subset])
			b.WriteString("}\n")
		}
	}
	return b.String()
}
