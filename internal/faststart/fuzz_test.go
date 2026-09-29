package faststart_test

import (
	"context"
	"errors"
	"testing"

	"github.com/skylab-kulubu/core-backend/internal/faststart"
	"github.com/skylab-kulubu/core-backend/internal/faststart/mp4test"
)

// fuzzLimits keep what a fuzzed file may make the rewrite read small.
var fuzzLimits = faststart.Limits{MaxMoovBytes: 1 << 16, MaxTopLevelBoxes: 64, MaxMoovBoxes: 1000}

// Whatever the file, the rewrite never panics and never reads outside it:
// it refuses the file (ErrInvalid), finds nothing to move
// (ErrAlreadyFaststart), or plans a rewrite whose bytes Verify takes and
// which is faststart itself. The seeds (the fixtures and every refusal)
// run as ordinary tests with go test; go test -fuzz=FuzzPlan explores
// beyond them.
func FuzzPlan(f *testing.F) {
	for _, m := range []mp4test.Movie{
		mp4test.TwoTracks(),
		{Chunks: mp4test.TwoTracks().Chunks, Tracks: mp4test.TwoTracks().Tracks, Wide: true, LargeMdat: true},
		{Chunks: mp4test.TwoTracks().Chunks, Tracks: mp4test.TwoTracks().Tracks,
			Before: [][]byte{mp4test.Box("free", make([]byte, 13))}, After: [][]byte{mp4test.Box("free", make([]byte, 7))}},
	} {
		file, _ := m.Build()
		f.Add(file)
	}
	for _, r := range refusals() {
		f.Add(r.file)
	}
	f.Fuzz(func(t *testing.T, file []byte) {
		ctx := context.Background()
		layout, err := faststart.Plan(ctx, mp4test.Memory(file), int64(len(file)), fuzzLimits)
		if errors.Is(err, faststart.ErrInvalid) || errors.Is(err, faststart.ErrAlreadyFaststart) {
			return
		}
		if err != nil {
			t.Fatalf("Plan failed on its own source: %v", err)
		}
		out, err := layout.Read(ctx, mp4test.Memory(file), 0, layout.Size)
		if err != nil || int64(len(out)) != layout.Size {
			t.Fatalf("read %d of %d bytes: %v", len(out), layout.Size, err)
		}
		if err := faststart.Verify(ctx, mp4test.Memory(out), mp4test.Memory(file), layout, fuzzLimits); err != nil {
			t.Fatalf("the rewrite fails its own check: %v", err)
		}
		if _, err := faststart.Plan(ctx, mp4test.Memory(out), layout.Size, fuzzLimits); !errors.Is(err, faststart.ErrAlreadyFaststart) {
			t.Fatalf("the rewrite is not faststart: %v", err)
		}
	})
}
