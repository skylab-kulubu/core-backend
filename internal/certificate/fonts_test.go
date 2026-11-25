package certificate

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"strconv"
	"strings"
	"testing"

	"github.com/andybalholm/brotli"
)

const turkishLetters = "ğĞıİşŞçÇöÖüÜ"

// Every face of every catalog font draws the Turkish letters and basic
// Latin, read from the files' cmap: a font without them does not belong
// in the catalog.
func TestCatalogFontsDrawTurkish(t *testing.T) {
	t.Parallel()
	for _, font := range catalogFonts {
		faces := map[string]map[rune]bool{}
		drawn := map[string]map[rune]bool{}
		names, err := fs.Glob(catalogFiles, path.Join("fonts", font.Dir, "*.woff2"))
		if err != nil || len(names) == 0 {
			t.Fatalf("%s: no files: %v", font.Family, err)
		}
		for _, name := range names {
			data, err := catalogFiles.ReadFile(name)
			if err != nil {
				t.Fatal(err)
			}
			runes, err := woff2Runes(data)
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			base := path.Base(name)
			face := base[:strings.LastIndex(base, "-latin")]
			// A letter is drawn only from a file whose unicode-range holds
			// it (ı is in both ranges and in the latin file alone).
			subset := strings.TrimSuffix(base[len(face)+1:], ".woff2")
			if drawn[face] == nil {
				drawn[face] = map[rune]bool{}
			}
			for r := range runes {
				if inUnicodeRange(subsetRanges[subset], r) {
					drawn[face][r] = true
				}
			}
			if faces[face] == nil {
				faces[face] = map[rune]bool{}
			}
			for r := range runes {
				faces[face][r] = true
			}
		}
		for face, runes := range faces {
			for _, r := range turkishLetters + "AZaz09.,'-" {
				if !runes[r] {
					t.Errorf("%s %s does not draw %q", font.Family, face, r)
				} else if !drawn[face][r] {
					t.Errorf("%s %s has %q only in a file whose unicode-range leaves it out", font.Family, face, r)
				}
			}
		}
	}
}

// Each font has its licence, 400 and 700 (a script font 400 alone), an
// italic 400 when the family has one, and both subsets of each face.
func TestCatalogFontsAreComplete(t *testing.T) {
	t.Parallel()
	script := map[string]bool{"greatvibes": true, "dancingscript": true}
	if len(catalogFonts) != 10 {
		t.Fatalf("%d catalog fonts, want 10", len(catalogFonts))
	}
	for _, font := range catalogFonts {
		if _, err := catalogFiles.ReadFile(path.Join("fonts", font.Dir, "OFL.txt")); err != nil {
			t.Errorf("%s: no OFL.txt: %v", font.Family, err)
		}
		css, err := font.faceCSS()
		if err != nil {
			t.Fatal(err)
		}
		want := []string{"font-style:normal;font-weight:400"}
		if !script[font.Dir] {
			want = append(want, "font-style:italic;font-weight:400;")
			if !strings.Contains(css, "font-style:normal;font-weight:400 700;") {
				want = append(want, "font-style:normal;font-weight:400;", "font-style:normal;font-weight:700;")
			}
		}
		for _, w := range want {
			if strings.Count(css, w) != 2 {
				t.Errorf("%s: %q in %d rules, want 2 (latin and latin-ext)", font.Family, w, strings.Count(css, w))
			}
		}
		if strings.Contains(css, "unicode-range:;") || strings.Contains(css, "unicode-range:}") {
			t.Errorf("%s: a file of an unknown subset", font.Family)
		}
		if got := safeFont(strings.ToUpper(font.Family)); got != font.Stack {
			t.Errorf("safeFont(%q) = %q, want %q", font.Family, got, font.Stack)
		}
	}
}

// A page embeds the catalog fonts its text uses and no other; a layout
// that uses none prints as before (TestLegacyLayoutIsStoredAndPrintedAsBefore).
func TestLayoutHTMLEmbedsTheFontsItUses(t *testing.T) {
	t.Parallel()
	layout, assets := legacyFixture(t)
	layout.Elements[0].FontFamily = "montserrat"
	layout.Elements[1].FontFamily = "Great Vibes"
	layout.Elements[2].FontFamily = "Montserrat"
	layout.Elements[4].FontFamily = "Poppins" // the QR code draws no text
	html, err := LayoutHTML(t.Context(), layout, legacyPreview, "https://skyl.app/c/X", assets)
	if err != nil {
		t.Fatal(err)
	}
	for family, want := range map[string]int{"Montserrat": 4, "Great Vibes": 2, "Poppins": 0, "Roboto": 0, "Inter": 0} {
		if got := strings.Count(html, "@font-face{font-family:'"+family+"'"); got != want {
			t.Errorf("%s: %d @font-face rules, want %d", family, got, want)
		}
	}
	if !strings.Contains(html, "font-family:Montserrat,'Liberation Sans',sans-serif;") || !strings.Contains(html, "font-family:'Great Vibes','Liberation Serif',cursive;") {
		t.Fatalf("the elements do not name their fonts: %s", html[strings.Index(html, "<main"):])
	}
	if !strings.Contains(html, "document.fonts.ready.then(function(){try{") || !strings.Contains(html, "}finally{"+fitDoneFlag+"}})") || !waitsForFit(html) {
		t.Fatal("the fit script measures text before the embedded fonts are in, or does not tell the renderer it is done")
	}

	layout, _ = legacyFixture(t)
	layout.Elements[0].FontFamily = "Noto Sans Display"
	layout.Elements[1].FontFamily = "Brandon Grotesque"
	html, err = LayoutHTML(t.Context(), layout, legacyPreview, "https://skyl.app/c/X", assets)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(html, "@font-face") || strings.Contains(html, "document.fonts") || waitsForFit(html) {
		t.Fatal("a page without catalog fonts embeds fonts or waits for them")
	}
	if !strings.Contains(html, "font-family:'Liberation Sans',Arial,sans-serif;") {
		t.Fatal("an unknown font no longer falls back to Liberation Sans")
	}
}

// woff2Runes is the characters a WOFF2 font maps, read from its cmap
// (format 4 or 12), which WOFF2 keeps untransformed.
func woff2Runes(data []byte) (map[rune]bool, error) {
	if len(data) < 48 || string(data[:4]) != "wOF2" {
		return nil, errors.New("not a WOFF2")
	}
	if string(data[4:8]) == "ttcf" {
		return nil, errors.New("a collection")
	}
	numTables := int(binary.BigEndian.Uint16(data[12:]))
	compressed := int(binary.BigEndian.Uint32(data[20:]))
	r := bytes.NewReader(data[48:])
	base128 := func() (int, error) {
		v := 0
		for i := 0; i < 5; i++ {
			b, err := r.ReadByte()
			if err != nil {
				return 0, err
			}
			v = v<<7 | int(b&0x7f)
			if b&0x80 == 0 {
				return v, nil
			}
		}
		return 0, errors.New("bad UIntBase128")
	}
	cmapAt, cmapLen, offset := -1, 0, 0
	for i := 0; i < numTables; i++ {
		flags, err := r.ReadByte()
		if err != nil {
			return nil, err
		}
		tag := int(flags & 0x3f)
		if tag == 63 {
			if _, err := r.Seek(4, io.SeekCurrent); err != nil {
				return nil, err
			}
		}
		length, err := base128()
		if err != nil {
			return nil, err
		}
		version := flags >> 6
		if (tag == 10 || tag == 11) == (version == 0) { // glyf/loca transformed at 0, others at non-0
			if length, err = base128(); err != nil {
				return nil, err
			}
		}
		if tag == 0 { // cmap
			cmapAt, cmapLen = offset, length
		}
		offset += length
	}
	start := len(data[48:]) - r.Len() + 48
	if cmapAt < 0 || start+compressed > len(data) {
		return nil, errors.New("no cmap")
	}
	tables, err := io.ReadAll(brotli.NewReader(bytes.NewReader(data[start : start+compressed])))
	if err != nil {
		return nil, err
	}
	if cmapAt+cmapLen > len(tables) {
		return nil, errors.New("short table data")
	}
	return cmapRunes(tables[cmapAt : cmapAt+cmapLen])
}

func cmapRunes(cmap []byte) (map[rune]bool, error) {
	u16 := func(b []byte, at int) int { return int(binary.BigEndian.Uint16(b[at:])) }
	u32 := func(b []byte, at int) int { return int(binary.BigEndian.Uint32(b[at:])) }
	runes := map[rune]bool{}
	for i := 0; i < u16(cmap, 2); i++ {
		rec := 4 + i*8
		platform, encoding, at := u16(cmap, rec), u16(cmap, rec+2), u32(cmap, rec+4)
		if !(platform == 0 || (platform == 3 && (encoding == 1 || encoding == 10))) {
			continue
		}
		switch sub := cmap[at:]; u16(sub, 0) {
		case 4:
			segs := u16(sub, 6) / 2
			ends, starts, deltas, offsets := 14, 16+segs*2, 16+segs*4, 16+segs*6
			for s := 0; s < segs; s++ {
				end, begin := u16(sub, ends+s*2), u16(sub, starts+s*2)
				delta, rangeOffset := u16(sub, deltas+s*2), u16(sub, offsets+s*2)
				for c := begin; c <= end && c != 0xFFFF; c++ {
					glyph := (c + delta) & 0xFFFF
					if rangeOffset != 0 {
						at := offsets + s*2 + rangeOffset + (c-begin)*2
						if glyph = u16(sub, at); glyph != 0 {
							glyph = (glyph + delta) & 0xFFFF
						}
					}
					if glyph != 0 {
						runes[rune(c)] = true
					}
				}
			}
		case 12:
			for g := 0; g < u32(sub, 12); g++ {
				group := 16 + g*12
				for c := u32(sub, group); c <= u32(sub, group+4); c++ {
					runes[rune(c)] = true
				}
			}
		default:
			continue
		}
	}
	if len(runes) == 0 {
		return nil, fmt.Errorf("no Unicode cmap subtable")
	}
	return runes, nil
}

// inUnicodeRange is r inside a CSS unicode-range ("U+0000-00FF,U+0131").
func inUnicodeRange(ranges string, r rune) bool {
	for _, part := range strings.Split(ranges, ",") {
		lo, hi, _ := strings.Cut(strings.TrimPrefix(strings.TrimSpace(part), "U+"), "-")
		if hi == "" {
			hi = lo
		}
		a, err1 := strconv.ParseUint(lo, 16, 32)
		b, err2 := strconv.ParseUint(hi, 16, 32)
		if err1 == nil && err2 == nil && uint64(r) >= a && uint64(r) <= b {
			return true
		}
	}
	return false
}
