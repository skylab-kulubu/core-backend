package theme_test

import (
	"strings"
	"testing"

	"github.com/skylab-kulubu/core-backend/internal/ticket/theme"
)

// The same code draws the same pattern; another code another.
func TestGuillocheIsDeterministic(t *testing.T) {
	t.Parallel()
	a := theme.Guilloche("K7M2QX9F", 186, 128, "#1F4E79")
	if a != theme.Guilloche("K7M2QX9F", 186, 128, "#1F4E79") {
		t.Fatal("not deterministic")
	}
	if a == theme.Guilloche("AB12CD34", 186, 128, "#1F4E79") {
		t.Fatal("two codes, one pattern")
	}
	if !strings.HasPrefix(a, "<svg ") || !strings.HasSuffix(a, "</svg>") || strings.Count(a, "<path ") != 4 {
		t.Fatalf("shape: %.120s", a)
	}
	// Small enough for a PDF of a few hundred kilobytes.
	if len(a) > 100000 {
		t.Fatalf("%d bytes", len(a))
	}
}

func TestLookupFallsBackToKurumsal(t *testing.T) {
	t.Parallel()
	for _, key := range []string{"", "kurumsal", "bilinmeyen", "ARTLAB"} {
		if got := theme.Lookup(key); got.Key != theme.Default {
			t.Errorf("%q: %q", key, got.Key)
		}
	}
	for key, want := range map[string]bool{"kurumsal": true, "artlab": true, "a-b-1": true, "": false, "ArtLab": false, strings.Repeat("a", 33): false, "art lab": false} {
		if theme.ValidKey(key) != want {
			t.Errorf("ValidKey(%q)", key)
		}
	}
}

func TestMarkPaths(t *testing.T) {
	t.Parallel()
	if paths := theme.MarkPaths(); len(paths) != 19 || !strings.HasPrefix(paths[0], "M") {
		t.Fatalf("%d paths", len(paths))
	}
}
