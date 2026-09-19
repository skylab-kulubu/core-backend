package certificate

import "testing"

func TestSafeFontSupportsCertificateFamilies(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"Arial":                 "'Liberation Sans',Arial,sans-serif",
		"Helvetica":             "'Liberation Sans',Helvetica,sans-serif",
		"Carlito":               "Carlito,'Liberation Sans',sans-serif",
		"Noto Sans Display":     "'Noto Sans Display','Noto Sans',sans-serif",
		"DejaVu Sans Condensed": "'DejaVu Sans Condensed','DejaVu Sans',sans-serif",
		"Times New Roman":       "'Liberation Serif','Times New Roman',serif",
		"Caladea":               "Caladea,'Liberation Serif',serif",
		"Noto Serif Display":    "'Noto Serif Display','Noto Serif',serif",
		"Courier New":           "'Liberation Mono','Courier New',monospace",
		"DejaVu Sans Mono":      "'DejaVu Sans Mono','Liberation Mono',monospace",
	}
	for input, want := range cases {
		input, want := input, want
		t.Run(input, func(t *testing.T) {
			t.Parallel()
			if got := safeFont(input); got != want {
				t.Fatalf("safeFont(%q) = %q, want %q", input, got, want)
			}
		})
	}
	if got := safeFont("url(javascript:alert(1))"); got != "'Liberation Sans',Arial,sans-serif" {
		t.Fatalf("unsafe font fallback = %q", got)
	}
}
