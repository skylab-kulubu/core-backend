package certificate

import (
	"embed"
	"encoding/base64"
	"io/fs"
	"path"
	"sort"
	"strings"
	"sync"
)

// catalogFiles are the catalog's font files, each named
// <style>-<weight>[-<weight>]-<subset>.woff2: Google Fonts' latin and
// latin-ext subsets, which together hold every Turkish letter. A file
// with two weights is a variable font that draws both and those between.
//
//go:embed fonts
var catalogFiles embed.FS

// subsetRanges are the unicode-range of each subset, as Google Fonts
// serves them.
var subsetRanges = map[string]string{
	"latin":     "U+0000-00FF,U+0131,U+0152-0153,U+02BB-02BC,U+02C6,U+02DA,U+02DC,U+0304,U+0308,U+0329,U+2000-206F,U+20AC,U+2122,U+2191,U+2193,U+2212,U+2215,U+FEFF,U+FFFD",
	"latin-ext": "U+0100-02BA,U+02BD-02C5,U+02C7-02CC,U+02CE-02D7,U+02DD-02FF,U+0304,U+0308,U+0329,U+1D00-1DBF,U+1E00-1E9F,U+1EF2-1EFF,U+2020,U+20A0-20AB,U+20AD-20C4,U+2113,U+2C60-2C7F,U+A720-A7FF",
}

// catalogFont is a font core embeds in the page that uses it: the
// renderer (Gotenberg) does not have it.
type catalogFont struct {
	Family string // the name a layout uses and the page's font-family
	Dir    string // its folder under fonts/
	Stack  string // the element's font-family, its fallbacks after it
}

// Lato is not one: Google Fonts' Lato has no ğ, ş or İ.
var catalogFonts = []catalogFont{
	{"Montserrat", "montserrat", `Montserrat,'Liberation Sans',sans-serif`},
	{"Poppins", "poppins", `Poppins,'Liberation Sans',sans-serif`},
	{"Open Sans", "opensans", `'Open Sans','Liberation Sans',sans-serif`},
	{"Roboto", "roboto", `Roboto,'Liberation Sans',sans-serif`},
	{"Inter", "inter", `Inter,'Liberation Sans',sans-serif`},
	{"Raleway", "raleway", `Raleway,'Liberation Sans',sans-serif`},
	{"Playfair Display", "playfairdisplay", `'Playfair Display','Liberation Serif',serif`},
	{"Merriweather", "merriweather", `Merriweather,'Liberation Serif',serif`},
	{"Great Vibes", "greatvibes", `'Great Vibes','Liberation Serif',cursive`},
	{"Dancing Script", "dancingscript", `'Dancing Script','Liberation Serif',cursive`},
}

// catalogFontNamed is the catalog font a layout names, by its family in any
// case.
func catalogFontNamed(family string) (catalogFont, bool) {
	key := strings.ToLower(strings.TrimSpace(family))
	for _, font := range catalogFonts {
		if strings.ToLower(font.Family) == key {
			return font, true
		}
	}
	return catalogFont{}, false
}

// A page that embeds fonts fits its text once they are in, after the page
// loaded, and then sets fitDoneFlag; the renderer prints it once
// fitDoneExpression is true (Gotenberg's waitForExpression).
const (
	fitDoneFlag       = "window.__certFitDone=true"
	fitDoneExpression = "window.__certFitDone === true"
)

var (
	fontFaceCSS     = map[string]string{}
	fontFaceCSSLock sync.Mutex
)

// faceCSS is a catalog font's @font-face rules, its files as data URIs;
// built once.
func (f catalogFont) faceCSS() (string, error) {
	fontFaceCSSLock.Lock()
	defer fontFaceCSSLock.Unlock()
	if css, ok := fontFaceCSS[f.Dir]; ok {
		return css, nil
	}
	names, err := fs.Glob(catalogFiles, path.Join("fonts", f.Dir, "*.woff2"))
	if err != nil {
		return "", err
	}
	sort.Strings(names)
	var css strings.Builder
	for _, name := range names {
		// normal-400-700-latin-ext.woff2 → normal, 400 700, latin-ext
		parts := strings.SplitN(strings.TrimSuffix(path.Base(name), ".woff2"), "-", 3)
		weight, subset := parts[1], parts[2]
		if hi, rest, ok := strings.Cut(subset, "-"); ok && hi != "" && hi[0] >= '0' && hi[0] <= '9' {
			weight, subset = weight+" "+hi, rest
		}
		data, err := catalogFiles.ReadFile(name)
		if err != nil {
			return "", err
		}
		css.WriteString(`@font-face{font-family:'` + f.Family + `';font-style:` + parts[0] + `;font-weight:` + weight +
			`;src:url(data:font/woff2;base64,` + base64.StdEncoding.EncodeToString(data) + `) format('woff2');unicode-range:` + subsetRanges[subset] + `}`)
	}
	fontFaceCSS[f.Dir] = css.String()
	return fontFaceCSS[f.Dir], nil
}

// fontFaces is the @font-face rules of the fonts the layout's text uses,
// none for a font the renderer has: a page carries only what it draws.
// assets is where an uploaded font is read from (the catalog's are built
// in).
func fontFaces(layout Layout, assets AssetReader) (string, error) {
	var css strings.Builder
	seen := map[string]bool{}
	for _, el := range layout.Elements {
		switch el.Kind {
		case "image", "shape", "verificationQr":
			continue
		}
		font, ok := catalogFontNamed(el.FontFamily)
		if !ok || seen[font.Dir] {
			continue
		}
		seen[font.Dir] = true
		faces, err := font.faceCSS()
		if err != nil {
			return "", err
		}
		css.WriteString(faces)
	}
	return css.String(), nil
}
