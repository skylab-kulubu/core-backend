package theme

import (
	"embed"
	"encoding/base64"
	"strconv"
	"strings"
	"sync"
)

// ARTLAB's theme: the club's AI summit (8. ARTLAB, 2026-11-27). The board
// chose variant B (Yusuf, 2026-11-26; notes/ekran/2026-11-26/
// artlab-pdf-varyasyon/B.png): the whole page dark, the poster's orange and
// electric blue, headings in Exo 2. The QR stays black on white, in a white
// box with 2 mm more white around it, at the size and in the cell it has in
// every theme. Everything ARTLAB draws is in this file: another variant is
// a change here alone.

// The theme's colours.
const (
	artlabPaper  = "#0A1222"
	artlabPanel  = "#0F1A2E"
	artlabLine   = "#2B3D5C"
	artlabOrange = "#FF8A1F"
	artlabBlue   = "#3AA0FF"
	artlabInk    = "#EAF2FF"
	artlabMuted  = "#8FA3BF"
)

// ArtlabHeading is the CSS font stack of ARTLAB's headings: Exo 2 (SIL Open
// Font License 1.1, fonts/OFL-Exo2.txt), embedded, then the body's. The
// poster's own face (Orbitron-like) lacks Turkish capitals; Exo 2 has them.
const ArtlabHeading = `'Exo 2','Manrope','Noto Sans','Liberation Sans',Arial,sans-serif`

// PosterAspect is the poster's shape, width over height: ARTLAB's ticket
// prints the Event's whole ticket image (its "Ayrı görsel", a landscape
// poster) across the top, cut about its centre to 3 : 1.
const artlabPosterAspect = 3.0

var artlab = Theme{
	Key:        "artlab",
	Name:       "ARTLAB",
	Accent:     artlabOrange,
	Secondary:  artlabBlue,
	Ink:        artlabInk,
	Muted:      artlabMuted,
	Paper:      artlabPaper,
	Rule:       "#22324A",
	Sans:       kurumsal.Sans,
	HeadSans:   kurumsal.HeadSans,
	PageAccent: artlabOrange,
	Look: &Look{
		FontCSS:      exo2FaceCSS,
		PosterAspect: artlabPosterAspect,
		DocumentCSS: strings.Join([]string{
			// The ground to the page's edge, its margins too (Chromium paints
			// the page box's background under the page head).
			`@page{background:` + artlabPaper + `}`,
			`:root{--accent2:` + artlabBlue + `;--line:` + artlabLine + `;--panel:` + artlabPanel + `}`,
			`.kapak{border-radius:2mm}`,
			`.kapak.afis{height:auto;aspect-ratio:3/1}`,
			`h1{font-family:` + ArtlabHeading + `;font-weight:900;font-size:17pt;letter-spacing:.14em;color:var(--accent);margin:5.5mm 0 3.5mm;display:flex;align-items:center;gap:3mm}`,
			`h1::after{content:"";flex:1;height:1.2pt;background:linear-gradient(90deg,var(--accent),var(--accent2))}`,
			`.izgara{border-top:.6pt solid var(--line);border-left:.6pt solid var(--line);background:var(--panel)}`,
			`.h{border-right:.6pt solid var(--line);border-bottom:.6pt solid var(--line)}`,
			`.h>span:not(.kod){color:var(--accent2);text-transform:uppercase;letter-spacing:.12em;font-size:6.8pt;font-weight:700}`,
			`.h .buyuk{font-family:` + ArtlabHeading + `;font-weight:800;font-size:14pt;letter-spacing:.02em}`,
			// The QR as in every theme (44 mm, its cell), on white with 2 mm
			// more white around it; the cell's sides give the box room.
			`.h.qr{background:var(--paper);padding-left:2.4mm;padding-right:2.4mm}`,
			`.qr img{background:#fff;padding:2mm;border-radius:1.5mm;box-sizing:content-box}`,
			`.kod{font-family:` + ArtlabHeading + `;color:var(--accent);font-size:12pt;letter-spacing:.12em}`,
			`section>h2{font-family:` + ArtlabHeading + `;font-weight:800;text-transform:uppercase;letter-spacing:.16em;color:var(--accent);border-bottom:.8pt solid var(--line)}`,
			`.gun h3{color:var(--ink)}`,
			`td{border-bottom-color:var(--line)}`,
			`td.saat{color:var(--accent2);font-weight:600}`,
			`td.kisi{color:var(--muted)}`,
			`.aciklama{color:#C9D6EA}`,
			`.yer .adres{color:var(--muted)}`,
			// The map darkened by its opacity over the dark ground: a PDF
			// draws that with the image as it is (a CSS filter would be
			// rasterized, a larger file).
			`.yer .harita{border-color:var(--line);opacity:.62}`,
			`.yer figure{width:26.4mm}`,
			`.yer figure img{background:#fff;padding:1.2mm;box-sizing:content-box;border-radius:1mm}`,
			`.dip{border-top:.6pt solid var(--line);color:var(--muted)}`,
		}, ""),
		// No picture: a band of the theme, dark with the poster's orange to
		// blue line along its foot.
		Band: `background:linear-gradient(90deg,` + artlabOrange + `,` + artlabBlue + `) left bottom/100% .9mm no-repeat,linear-gradient(105deg,` +
			artlabPanel + ` 0%,#16305A 55%,` + artlabPaper + ` 100%)`,
		HeadInk:   artlabInk,
		HeadMuted: artlabMuted,
		HeadRule:  "1pt solid " + artlabOrange,
		HeadCSS:   `.isaret{width:6.6mm}`,
		HeadName:  "ARTLAB",
		HeadSub:   "SKY LAB",
	},
}

//go:embed artlab-icon.svg
var artlabIconSVG string

// ArtlabIcon is ARTLAB's icon (artlab-site src/app/icon.svg) on the page's
// ground, sized by its box.
func ArtlabIcon() string {
	icon := strings.ReplaceAll(strings.TrimSpace(artlabIconSVG), "#0B0E13", artlabPaper)
	icon = strings.ReplaceAll(icon, "\n", "")
	return strings.Replace(icon, "<svg ", `<svg width="100%" height="100%" aria-hidden="true" `, 1)
}

func init() {
	artlab.Look.HeadMark = ArtlabIcon()
}

// The embedded Exo 2: its two heaviest static weights (ARTLAB's headings),
// each in its Latin and Latin Extended subsets (Turkish needs both), as
// Fontsource 5.3.0 publishes them. Static fonts stay text in a PDF.
//
//go:embed fonts/*.woff2
var exo2Files embed.FS

// Exo2Weights are the weights of Exo 2 embedded.
var Exo2Weights = []int{800, 900}

// Subsets are the subsets' unicode ranges, as Fontsource's CSS gives them
// (the same as brandfont's).
var Subsets = map[string]string{
	"latin":     "U+0000-00FF,U+0131,U+0152-0153,U+02BB-02BC,U+02C6,U+02DA,U+02DC,U+0304,U+0308,U+0329,U+2000-206F,U+20AC,U+2122,U+2191,U+2193,U+2212,U+2215,U+FEFF,U+FFFD",
	"latin-ext": "U+0100-02BA,U+02BD-02C5,U+02C7-02CC,U+02CE-02D7,U+02DD-02FF,U+0304,U+0308,U+0329,U+1D00-1DBF,U+1E00-1E9F,U+1EF2-1EFF,U+2020,U+20A0-20AB,U+20AD-20C0,U+2113,U+2C60-2C7F,U+A720-A7FF",
}

var (
	exo2Once sync.Once
	exo2CSS  string
)

// Exo2Files is the embedded Exo 2 files, for the tests.
func Exo2Files() embed.FS { return exo2Files }

// exo2FaceCSS is the @font-face rules of the embedded Exo 2, each file a
// data URL.
func exo2FaceCSS() string {
	exo2Once.Do(func() {
		var b strings.Builder
		for _, weight := range Exo2Weights {
			for _, subset := range []string{"latin-ext", "latin"} {
				name := "fonts/exo-2-" + subset + "-" + strconv.Itoa(weight) + "-normal.woff2"
				data, err := exo2Files.ReadFile(name)
				if err != nil {
					panic("theme: embedded font missing: " + name)
				}
				b.WriteString("@font-face{font-family:'Exo 2';font-style:normal;font-display:block;font-weight:")
				b.WriteString(strconv.Itoa(weight))
				b.WriteString(";src:url(data:font/woff2;base64,")
				b.WriteString(base64.StdEncoding.EncodeToString(data))
				b.WriteString(") format('woff2');unicode-range:")
				b.WriteString(Subsets[subset])
				b.WriteString("}\n")
			}
		}
		exo2CSS = b.String()
	})
	return exo2CSS
}
