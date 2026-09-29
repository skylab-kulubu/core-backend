package media_test

import (
	"testing"

	"github.com/google/uuid"

	"github.com/skylab-kulubu/core-backend/internal/media"
)

// A video's faststart copies sit beside it, one per claim that writes one
// (videos/<uuid>.fs.<claim id>.mp4, still an .mp4 players take), under one
// prefix; every key of a video names its original. No other key does.
func TestAVideosKeysNameItsOriginalAndItsCopies(t *testing.T) {
	t.Parallel()
	const original = "videos/1c07.mp4"
	claim := uuid.MustParse("5e9d0000-0000-4000-8000-00000000abcd")
	copyKey, ok := media.FaststartCopyKey(original, claim)
	if !ok || copyKey != "videos/1c07.fs.5e9d000000004000800000000000abcd.mp4" {
		t.Fatalf("copy key %q %v", copyKey, ok)
	}
	if other, _ := media.FaststartCopyKey(original, uuid.New()); other == copyKey {
		t.Fatal("two claims share a copy key")
	}
	for _, key := range []string{original, copyKey} {
		if got, ok := media.VideoOriginalOf(key); !ok || got != original {
			t.Fatalf("original of %s: %q %v", key, got, ok)
		}
		if got, ok := media.CopiesPrefix(key); !ok || got != "videos/1c07.fs." {
			t.Fatalf("copies of %s under %q %v", key, got, ok)
		}
	}
	if _, ok := media.FaststartCopyKey(copyKey, claim); ok {
		t.Fatal("a faststart copy has a faststart copy of its own")
	}
	for _, key := range []string{"files/1c07", "videos/1c07", "videos/.mp4", "videos/.fs.x.mp4", "videos/a.fs..mp4", "videos/a.fs.x.y.mp4", "videos/a/b.mp4", "images/1c07.mp4", "https://cdn.example/videos/1c07.mp4"} {
		if original, ok := media.VideoOriginalOf(key); ok {
			t.Errorf("%s names an original %s", key, original)
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
