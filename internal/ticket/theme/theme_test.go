package theme_test

import (
	"strings"
	"testing"

	"github.com/skylab-kulubu/core-backend/internal/ticket/theme"
)

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
