package certificate

import "testing"

// The PDF font → layout font table (SPEC §5.3): the subset prefix goes,
// the weight and italic come from the name or the font's description,
// known names map to the catalog or the renderer's family, the rest to a
// family of their class.
func TestMapFont(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		font        PDFFont
		family      string
		weight      int
		style       string
		substituted bool
	}{
		{PDFFont{Name: "ABCDEF+Montserrat-Bold", Weight: 700}, "Montserrat", 700, "", false},
		{PDFFont{Name: "Montserrat-SemiBoldItalic"}, "Montserrat", 600, "italic", false},
		{PDFFont{Name: "Montserrat Bold", Weight: 700}, "Montserrat", 700, "", false},
		// The weight is read from the style part only.
		{PDFFont{Name: "Montserrat Bold"}, "Montserrat", 400, "", false},
		{PDFFont{Name: "Blackadder-Regular"}, "Noto Sans", 400, "", true},
		{PDFFont{Name: "OpenSans-Regular", Weight: 400}, "Open Sans", 400, "", false},
		{PDFFont{Name: "PlayfairDisplay-Italic", Italic: true}, "Playfair Display", 400, "italic", false},
		{PDFFont{Name: "Poppins-ExtraBold"}, "Poppins", 800, "", false},
		{PDFFont{Name: "Inter-Regular"}, "Inter", 400, "", false},
		{PDFFont{Name: "Raleway-Medium"}, "Raleway", 500, "", false},
		{PDFFont{Name: "Roboto-Black"}, "Roboto", 900, "", false},
		{PDFFont{Name: "Merriweather-LightItalic"}, "Merriweather", 300, "italic", false},
		{PDFFont{Name: "GreatVibes-Regular"}, "Great Vibes", 400, "", false},
		{PDFFont{Name: "DancingScript-Bold", Weight: 700}, "Dancing Script", 400, "", false},
		{PDFFont{Name: "ArialMT", Weight: 400}, "Arial", 400, "", false},
		{PDFFont{Name: "Arial-BoldMT", Weight: 700}, "Arial", 700, "", false},
		{PDFFont{Name: "TimesNewRomanPSMT"}, "Times New Roman", 400, "", false},
		{PDFFont{Name: "TimesNewRomanPS-BoldItalicMT"}, "Times New Roman", 700, "italic", false},
		{PDFFont{Name: "Helvetica-Bold", Weight: 700}, "Helvetica", 700, "", false},
		{PDFFont{Name: "Helvetica-Oblique"}, "Helvetica", 400, "italic", false},
		{PDFFont{Name: "HelveticaNeue-Bold"}, "Helvetica", 700, "", true},
		{PDFFont{Name: "CourierNewPSMT"}, "Courier New", 400, "", false},
		{PDFFont{Name: "Calibri,Bold"}, "Carlito", 700, "", true},
		{PDFFont{Name: "Cambria"}, "Caladea", 400, "", true},
		{PDFFont{Name: "DejaVuSans-Bold"}, "DejaVu Sans", 700, "", false},
		{PDFFont{Name: "AAAAAA+Font1", Family: "Montserrat", Weight: 700}, "Montserrat", 700, "", false},
		// Not known: by class.
		{PDFFont{Name: "BrandonGrotesque-Bold"}, "Noto Sans", 700, "", true},
		{PDFFont{Name: "CanvaSans-Regular"}, "Noto Sans", 400, "", true},
		{PDFFont{Name: "EBGaramond-Regular"}, "Noto Serif", 400, "", true},
		{PDFFont{Name: "Lora-Bold", Serif: true}, "Noto Serif", 700, "", true},
		{PDFFont{Name: "SourceSansPro-Bold", Serif: true}, "Noto Sans", 700, "", true},
		{PDFFont{Name: "FiraCode-Regular"}, "DejaVu Sans Mono", 400, "", true},
		{PDFFont{Name: "Unknown", Mono: true}, "DejaVu Sans Mono", 400, "", true},
		{PDFFont{Name: "AlexBrush-Regular"}, "Great Vibes", 400, "", true},
		{PDFFont{Name: "PinyonScript", Italic: true}, "Great Vibes", 400, "", true},
		{PDFFont{Name: "Allura-Regular"}, "Great Vibes", 400, "", true},
		{PDFFont{Name: "SnellRoundhand", Italic: true}, "Great Vibes", 400, "", true},
		{PDFFont{Name: "Signatura", Script: true}, "Great Vibes", 400, "", true},
		{PDFFont{Name: ""}, "Noto Sans", 400, "", true},
		// A description weight that is not a usual one (Chrome's PDFs) is
		// not taken.
		{PDFFont{Name: "Georgia-Italic", Weight: 645, Italic: true}, "Georgia", 400, "italic", true},
		{PDFFont{Name: "Georgia", Weight: 380}, "Georgia", 400, "", true},
	} {
		got := mapFont(tc.font, nil)
		if got.Family != tc.family || got.Weight != tc.weight || got.Style != tc.style || got.Substituted != tc.substituted {
			t.Errorf("%+v: %+v, want %s %d %q substituted=%v", tc.font, got, tc.family, tc.weight, tc.style, tc.substituted)
		}
		if _, ok := catalogFontNamed(got.Family); !ok && safeFont(got.Family) == safeFont("unknown family") && got.Family != "Liberation Sans" && got.Family != "Arial" {
			t.Errorf("%s is not a family the renderer draws", got.Family)
		}
	}
}

// An uploaded family (ticket 18) is taken before the catalog and the
// class fallback.
func TestMapFontPrefersUploaded(t *testing.T) {
	t.Parallel()
	got := mapFont(PDFFont{Name: "BrandonGrotesque-Bold"}, []string{"Brandon Grotesque"})
	if got.Family != "Brandon Grotesque" || got.Substituted || got.Weight != 700 {
		t.Fatalf("%+v", got)
	}
}
