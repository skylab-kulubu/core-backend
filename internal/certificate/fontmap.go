package certificate

import (
	"strings"
	"unicode"

	"github.com/google/uuid"
)

// fontMatch is the font a converted PDF text is set in: a family the
// renderer draws (safeFont: core's catalog or the renderer's own), its
// weight and style. Substituted is a family other than the PDF's, which
// the editor tells (warning fontSubstituted).
type fontMatch struct {
	Family      string
	Weight      int
	Style       string // "" or FontStyleItalic
	Substituted bool
	// MediaID is the certificate font's file when the family is one the
	// template's team or the club uploaded (Element.FontMediaID).
	MediaID *uuid.UUID
}

// fontAlias is a family the renderer draws for a PDF font family. alias:
// the same family by another name (ArialMT is Arial), not a substitute.
type fontAlias struct {
	family string
	alias  bool
}

// fontAliases maps a PDF font family, as familyKey spells it, to the
// family a layout names. Core's catalog families map to themselves
// (init); these are the renderer's families and their usual PDF names.
var fontAliases = map[string]fontAlias{
	"arial":           {"Arial", true},
	"helvetica":       {"Helvetica", true},
	"helveticaneue":   {"Helvetica", false},
	"arimo":           {"Arial", false},
	"liberationsans":  {"Liberation Sans", true},
	"timesnewroman":   {"Times New Roman", true},
	"times":           {"Times New Roman", true},
	"timesroman":      {"Times New Roman", true},
	"tinos":           {"Times New Roman", false},
	"liberationserif": {"Liberation Serif", true},
	"couriernew":      {"Courier New", true},
	"courier":         {"Courier New", true},
	"cousine":         {"Courier New", false},
	"liberationmono":  {"Liberation Mono", true},
	// The renderer has no Georgia: it prints Liberation Serif.
	"georgia":              {"Georgia", false},
	"calibri":              {"Carlito", false},
	"carlito":              {"Carlito", true},
	"cambria":              {"Caladea", false},
	"caladea":              {"Caladea", true},
	"notosans":             {"Noto Sans", true},
	"notosansdisplay":      {"Noto Sans Display", true},
	"notoserif":            {"Noto Serif", true},
	"notoserifdisplay":     {"Noto Serif Display", true},
	"dejavusans":           {"DejaVu Sans", true},
	"dejavusanscondensed":  {"DejaVu Sans Condensed", true},
	"dejavuserif":          {"DejaVu Serif", true},
	"dejavuserifcondensed": {"DejaVu Serif Condensed", true},
	"dejavusansmono":       {"DejaVu Sans Mono", true},
}

func init() {
	for _, font := range catalogFonts {
		fontAliases[familyKey(font.Family)] = fontAlias{font.Family, true}
	}
}

// The families a PDF font falls back to by its class when nothing above
// names it (SPEC §5.3).
const (
	fallbackSans   = "Noto Sans"
	fallbackSerif  = "Noto Serif"
	fallbackMono   = "DejaVu Sans Mono"
	fallbackScript = "Great Vibes"
)

// Words in a font's name that tell its class.
var (
	scriptWords = []string{"script", "vibes", "brush", "allura", "pinyon", "dancing", "signature", "handwrit", "calligra", "roundhand", "snell", "chancery", "zapfino", "cursive"}
	monoWords   = []string{"mono", "courier", "consol", "code"}
	serifWords  = []string{"serif", "garamond", "baskerville", "bodoni", "didot", "caslon", "minion", "times", "georgia", "playfair", "cormorant", "lora", "slab"}
)

// singleStyleFamilies have one weight and no italic: a heavier or slanted
// request would be faked by the browser, so it is not made.
var singleStyleFamilies = map[string]bool{"Great Vibes": true, "Dancing Script": true}

// styleSuffixes are the style words a font name may end with, longest
// first so "bolditalic" goes before "italic".
var styleSuffixes = []string{
	"extralightitalic", "semibolditalic", "extrabolditalic", "bolditalic", "lightitalic", "mediumitalic", "blackitalic",
	"extralight", "ultralight", "semibold", "demibold", "extrabold", "ultrabold",
	"italic", "oblique", "regular", "hairline", "medium", "light", "black", "heavy", "thin", "bold", "book", "normal",
}

// mapFont is the family, weight and style a PDF font is set in (SPEC §5.3):
// (1) a family of the template's team or the club uploaded (uploaded, their
// current certificate fonts: the face of the PDF's style nearest its
// weight, set at that face's weight and style), (2) the catalog or the
// renderer's font of that family or a known name of it (ArialMT is Arial,
// Montserrat-Bold is Montserrat 700), (3) by its class: a script font
// Great Vibes, a monospace one DejaVu Sans Mono, a serif one Noto Serif,
// any other Noto Sans. The PDF's embedded font file is never used: a
// subset holds only the letters the PDF prints.
func mapFont(font PDFFont, uploaded []Font) fontMatch {
	name := stripSubset(strings.TrimSpace(font.Name))
	// A weight word in the name wins; then the font's description, when
	// it is a usual weight (100 … 900, by hundreds): some producers write
	// a number derived from the stems there (Chrome: 645 for an italic
	// regular); else 400.
	weight := weightFromName(name)
	if weight == 0 {
		weight = 400
		if font.Weight >= 100 && font.Weight <= 900 && font.Weight%100 == 0 {
			weight = font.Weight
		}
	}
	match := fontMatch{Weight: weight}
	low := strings.ToLower(name)
	if font.Italic || strings.Contains(low, "italic") || strings.Contains(low, "oblique") {
		match.Style = FontStyleItalic
	}
	keys := []string{familyKey(font.Family), familyKey(name)}
	if face, ok := uploadedFace(keys, uploaded, match.Weight, match.Style); ok {
		id := face.MediaID
		match.Family, match.MediaID, match.Weight, match.Style = face.Family, &id, face.Weight, ""
		if face.Style == FontStyleItalic {
			match.Style = FontStyleItalic
		}
		return match
	}
	match.Family, match.Substituted = pickFamily(keys, font, low)
	if singleStyleFamilies[match.Family] {
		match.Weight, match.Style = 400, ""
	}
	return match
}

// uploadedFace is the certificate font of the font's family (by its keys)
// that is closest to weight and style: the same style first, then the
// nearest weight, the heavier of two as near.
func uploadedFace(keys []string, uploaded []Font, weight int, style string) (Font, bool) {
	var best Font
	found := false
	score := func(f Font) int {
		s := abs(f.Weight - weight)
		if f.Weight > weight {
			s-- // the heavier of two as near
		}
		fontItalic := f.Style == FontStyleItalic
		if fontItalic != (style == FontStyleItalic) {
			s += 10000
		}
		return s
	}
	for _, f := range uploaded {
		key := familyKey(f.Family)
		if key == "" || (key != keys[0] && key != keys[1]) {
			continue
		}
		if !found || score(f) < score(best) {
			best, found = f, true
		}
	}
	return best, found
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

// pickFamily is the family for the font's keys and whether it stands in
// for another family.
func pickFamily(keys []string, font PDFFont, low string) (string, bool) {
	for _, key := range keys {
		if alias, ok := fontAliases[key]; ok && key != "" {
			return alias.family, !alias.alias
		}
	}
	switch {
	case font.Script || containsAny(low, scriptWords):
		return fallbackScript, true
	case font.Mono || containsAny(low, monoWords):
		return fallbackMono, true
	case (font.Serif && !strings.Contains(low, "sans")) || (containsAny(low, serifWords) && !strings.Contains(low, "sans")):
		return fallbackSerif, true
	default:
		return fallbackSans, true
	}
}

// familyKey is a font family or PostScript name without its style and its
// vendor marks, in lower case letters and digits: "TimesNewRomanPS-
// BoldItalicMT", "Times New Roman" and "TimesNewRomanPSMT" are all
// "timesnewroman"; "Montserrat-Bold" and "Montserrat Bold" "montserrat".
func familyKey(name string) string {
	name = stripSubset(strings.TrimSpace(name))
	// A PostScript name is Family-Style; Word writes Family,Style.
	if i := strings.IndexAny(name, "-,"); i > 0 {
		name = name[:i]
	}
	var b strings.Builder
	for _, r := range strings.ToLower(name) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	key := b.String()
	for changed := true; changed; {
		changed = false
		for _, suffix := range append([]string{"psmt", "mt", "ps"}, styleSuffixes...) {
			if len(key) > len(suffix)+2 && strings.HasSuffix(key, suffix) {
				key = strings.TrimSuffix(key, suffix)
				changed = true
			}
		}
	}
	return key
}

// stripSubset drops a subset prefix (ABCDEF+).
func stripSubset(name string) string {
	if len(name) > 7 && name[6] == '+' {
		for _, r := range name[:6] {
			if r < 'A' || r > 'Z' {
				return name
			}
		}
		return name[7:]
	}
	return name
}

// weightWords are the weights a font's name tells, longest words first.
var weightWords = []struct {
	word   string
	weight int
}{
	{"extralight", 200}, {"ultralight", 200}, {"semibold", 600}, {"demibold", 600}, {"extrabold", 800}, {"ultrabold", 800},
	{"hairline", 100}, {"thin", 100}, {"light", 300}, {"medium", 500}, {"bold", 700}, {"black", 900}, {"heavy", 900},
}

// weightFromName is the weight a font's name tells in its style part, after
// "-" or "," (Montserrat-Bold, Calibri,Bold: Thin 100 … Black 900), 0 when
// it tells none: a family's own name (Blackadder, Arial Black) is not a
// weight.
func weightFromName(name string) int {
	i := strings.IndexAny(name, "-,")
	if i < 0 {
		return 0
	}
	low := strings.NewReplacer("-", "", " ", "", "_", "").Replace(strings.ToLower(name[i+1:]))
	for _, w := range weightWords {
		if strings.Contains(low, w.word) {
			return w.weight
		}
	}
	return 0
}

func containsAny(s string, words []string) bool {
	for _, word := range words {
		if strings.Contains(s, word) {
			return true
		}
	}
	return false
}
