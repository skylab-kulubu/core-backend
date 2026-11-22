package brandfont_test

import (
	"strings"
	"testing"

	"github.com/skylab-kulubu/core-backend/internal/brandfont"
)

// Every weight comes in both subsets, each a WOFF2 inline: Turkish's ı is
// in the Latin one, ğ and ş in Latin Extended.
func TestFaceCSS(t *testing.T) {
	t.Parallel()
	css := brandfont.FaceCSS()
	if got := strings.Count(css, "@font-face{font-family:'Manrope'"); got != 2*len(brandfont.Weights) {
		t.Fatalf("%d faces", got)
	}
	for _, want := range []string{"font-weight:400;", "font-weight:800;", "src:url(data:font/woff2;base64,d09GMg", "U+0131", "U+0100-02BA"} {
		if !strings.Contains(css, want) {
			t.Errorf("no %q", want)
		}
	}
	if strings.Contains(css, "http") {
		t.Error("a font from outside")
	}
}
