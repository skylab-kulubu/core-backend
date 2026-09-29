package media_test

import (
	"testing"

	"github.com/skylab-kulubu/core-backend/internal/media"
)

// A video's faststart copy sits beside it (videos/<uuid>.fs.mp4, still an
// .mp4 players take), and each of the two keys names the other, so a purge
// deletes both whichever the Media points at. No other key has a pair.
func TestAVideosTwoKeysNameEachOther(t *testing.T) {
	t.Parallel()
	const original, copy = "videos/1c07.mp4", "videos/1c07.fs.mp4"
	if got, ok := media.FaststartKeyOf(original); !ok || got != copy {
		t.Fatalf("faststart key of %s: %q %v", original, got, ok)
	}
	if got, ok := media.VideoPairKey(original); !ok || got != copy {
		t.Fatalf("pair of %s: %q %v", original, got, ok)
	}
	if got, ok := media.VideoPairKey(copy); !ok || got != original {
		t.Fatalf("pair of %s: %q %v", copy, got, ok)
	}
	if _, ok := media.FaststartKeyOf(copy); ok {
		t.Fatal("a faststart copy has a faststart copy of its own")
	}
	for _, key := range []string{"files/1c07", "videos/1c07", "videos/.mp4", "videos/.fs.mp4", "videos/a/b.mp4", "images/1c07.mp4", "https://cdn.example/videos/1c07.mp4"} {
		if other, ok := media.VideoPairKey(key); ok {
			t.Errorf("%s has a pair %s", key, other)
		}
	}
}
