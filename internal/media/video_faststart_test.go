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

// A copy is written in 16 MiB parts, unless that would take more than the
// 10 000 parts S3 allows: then in the fewest whole MiB that do not.
func TestFaststartPartSizeStaysWithinTenThousandParts(t *testing.T) {
	t.Parallel()
	w, err := media.NewFaststartWorker(media.FaststartWorkerConfig{Store: &media.PostgresStore{}, Storage: media.NewR2(media.R2Config{})})
	if err != nil {
		t.Fatal(err)
	}
	const mib, gib = 1 << 20, 1 << 30
	for size, want := range map[int64]int64{
		100:              16 * mib,
		2 * gib:          16 * mib,
		10000 * 16 * mib: 16 * mib,
		10000*16*mib + 1: 17 * mib,
		1 << 40:          105 * mib,
	} {
		if got := w.PartSizeFor(size); got != want || (size+got-1)/got > 10000 {
			t.Errorf("a copy of %d bytes: parts of %d, want %d", size, got, want)
		}
	}
}
