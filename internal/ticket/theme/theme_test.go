package theme_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"io/fs"
	"strconv"
	"strings"
	"testing"

	"github.com/andybalholm/brotli"
	"github.com/skylab-kulubu/core-backend/internal/ticket/theme"
)

func TestLookupFallsBackToKurumsal(t *testing.T) {
	t.Parallel()
	for _, key := range []string{"", "kurumsal", "bilinmeyen", "ARTLAB", " artlab-2025 "} {
		if got := theme.Lookup(key); got.Key != theme.Default {
			t.Errorf("%q: %q", key, got.Key)
		}
	}
	if got := theme.Lookup("artlab"); got.Key != "artlab" || got.Look == nil {
		t.Errorf("artlab: %+v", got)
	}
	for key, want := range map[string]bool{"kurumsal": true, "artlab": true, "a-b-1": true, "": false, "ArtLab": false, strings.Repeat("a", 33): false, "art lab": false} {
		if theme.ValidKey(key) != want {
			t.Errorf("ValidKey(%q)", key)
		}
	}
	for key, want := range map[string]bool{"kurumsal": true, "artlab": true, "bilinmeyen": false, "": false} {
		if theme.Known(key) != want {
			t.Errorf("Known(%q)", key)
		}
	}
}

// The catalogue the panel lists: the default first, each key the forms
// service's shape, each with its colours; Kurumsal draws nothing of its own.
func TestCatalog(t *testing.T) {
	t.Parallel()
	all := theme.Catalog()
	if len(all) != 2 || all[0].Key != theme.Default || all[1].Key != "artlab" {
		t.Fatalf("%+v", all)
	}
	for _, th := range all {
		if !theme.ValidKey(th.Key) || th.Name == "" || th.Paper == "" || th.Ink == "" || th.Accent == "" || th.Secondary == "" {
			t.Errorf("%+v", th)
		}
	}
	if all[0].Look != nil || all[0].PageAccent != "" {
		t.Error("Kurumsal draws a look of its own")
	}
}

// ARTLAB (variant B): the page's ground to its edges, the QR on white with
// 2 mm around it at its size, the poster's shape, the page head's mark.
func TestArtlabLook(t *testing.T) {
	t.Parallel()
	look := theme.Lookup("artlab").Look
	for _, want := range []string{"@page{background:#0A1222}", ".qr img{background:#fff;padding:2mm;", ".kapak.afis{height:auto;aspect-ratio:3/1}", "font-family:'Exo 2'"} {
		if !strings.Contains(look.DocumentCSS, want) {
			t.Errorf("no %q", want)
		}
	}
	if strings.Contains(look.DocumentCSS, ".qr img{width") || strings.Contains(look.DocumentCSS, "filter:") {
		t.Error("the QR's size changed, or the map is filtered (rasterized)")
	}
	if look.PosterAspect != 3 || look.Band == "" || look.HeadName != "ARTLAB" || !strings.HasPrefix(look.HeadMark, `<svg width="100%" height="100%"`) ||
		strings.Contains(look.HeadMark, "#0B0E13") || strings.Contains(look.HeadMark, "\n") {
		t.Errorf("%+v", look)
	}
	css := look.FontCSS()
	if got := strings.Count(css, "@font-face{font-family:'Exo 2'"); got != 2*len(theme.Exo2Weights) || strings.Contains(css, "http") {
		t.Errorf("%d faces", got)
	}
}

// Every embedded Exo 2 face draws Turkish, read from the files' cmap, each
// letter from the file whose unicode-range holds it: İ Ş Ğ in Latin
// Extended, ı in Latin, Ç Ö Ü in both.
func TestExo2DrawsTurkish(t *testing.T) {
	t.Parallel()
	files := theme.Exo2Files()
	for _, weight := range theme.Exo2Weights {
		drawn := map[rune]bool{}
		for subset, ranges := range theme.Subsets {
			data, err := fs.ReadFile(files, "fonts/exo-2-"+subset+"-"+strconv.Itoa(weight)+"-normal.woff2")
			if err != nil {
				t.Fatal(err)
			}
			runes, err := woff2Runes(data)
			if err != nil {
				t.Fatalf("%s %d: %v", subset, weight, err)
			}
			for r := range runes {
				if inUnicodeRange(ranges, r) {
					drawn[r] = true
				}
			}
		}
		for _, r := range "ğĞıİşŞçÇöÖüÜâÂîÎûÛ" + "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789:–·" {
			if !drawn[r] {
				t.Errorf("Exo 2 %d draws no %q", weight, r)
			}
		}
	}
}

// woff2Runes is the characters a WOFF2 font maps, from its cmap (format 4
// or 12), which WOFF2 keeps untransformed (as internal/certificate's test).
func woff2Runes(data []byte) (map[rune]bool, error) {
	if len(data) < 48 || string(data[:4]) != "wOF2" {
		return nil, errors.New("not a WOFF2")
	}
	numTables := int(binary.BigEndian.Uint16(data[12:]))
	compressed := int(binary.BigEndian.Uint32(data[20:]))
	r := bytes.NewReader(data[48:])
	base128 := func() (int, error) {
		v := 0
		for range 5 {
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
	for range numTables {
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
		if version := flags >> 6; (tag == 10 || tag == 11) == (version == 0) {
			if length, err = base128(); err != nil {
				return nil, err
			}
		}
		if tag == 0 {
			cmapAt, cmapLen = offset, length
		}
		offset += length
	}
	start := len(data[48:]) - r.Len() + 48
	if cmapAt < 0 || start+compressed > len(data) {
		return nil, errors.New("no cmap")
	}
	tables, err := io.ReadAll(brotli.NewReader(bytes.NewReader(data[start : start+compressed])))
	if err != nil || cmapAt+cmapLen > len(tables) {
		return nil, errors.New("short table data")
	}
	cmap := tables[cmapAt : cmapAt+cmapLen]
	u16 := func(b []byte, at int) int { return int(binary.BigEndian.Uint16(b[at:])) }
	u32 := func(b []byte, at int) int { return int(binary.BigEndian.Uint32(b[at:])) }
	runes := map[rune]bool{}
	for i := range u16(cmap, 2) {
		rec := 4 + i*8
		platform, encoding, at := u16(cmap, rec), u16(cmap, rec+2), u32(cmap, rec+4)
		if !(platform == 0 || (platform == 3 && (encoding == 1 || encoding == 10))) {
			continue
		}
		switch sub := cmap[at:]; u16(sub, 0) {
		case 4:
			segs := u16(sub, 6) / 2
			ends, starts, deltas, offsets := 14, 16+segs*2, 16+segs*4, 16+segs*6
			for s := range segs {
				end, begin := u16(sub, ends+s*2), u16(sub, starts+s*2)
				delta, rangeOffset := u16(sub, deltas+s*2), u16(sub, offsets+s*2)
				for c := begin; c <= end && c != 0xFFFF; c++ {
					glyph := (c + delta) & 0xFFFF
					if rangeOffset != 0 {
						if glyph = u16(sub, offsets+s*2+rangeOffset+(c-begin)*2); glyph != 0 {
							glyph = (glyph + delta) & 0xFFFF
						}
					}
					if glyph != 0 {
						runes[rune(c)] = true
					}
				}
			}
		case 12:
			for g := range u32(sub, 12) {
				group := 16 + g*12
				for c := u32(sub, group); c <= u32(sub, group+4); c++ {
					runes[rune(c)] = true
				}
			}
		}
	}
	if len(runes) == 0 {
		return nil, errors.New("no Unicode cmap subtable")
	}
	return runes, nil
}

// inUnicodeRange is r inside a CSS unicode-range ("U+0000-00FF,U+0131").
func inUnicodeRange(ranges string, r rune) bool {
	for part := range strings.SplitSeq(ranges, ",") {
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

func TestMarkPaths(t *testing.T) {
	t.Parallel()
	if paths := theme.MarkPaths(); len(paths) != 19 || !strings.HasPrefix(paths[0], "M") {
		t.Fatalf("%d paths", len(paths))
	}
}
