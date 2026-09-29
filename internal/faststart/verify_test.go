package faststart_test

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/skylab-kulubu/core-backend/internal/faststart"
	"github.com/skylab-kulubu/core-backend/internal/faststart/mp4test"
)

// A stored rewrite is checked against its plan before anything points at
// it: its moov first and exactly the rewritten one, and the chunks the plan
// probes holding the source's bytes. A copy that is not the rewrite (the
// source itself, a moov changed on the way, media data out of place) is a
// mismatch.
func TestVerifyTakesOnlyTheRewrite(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	file, _ := mp4test.TwoTracks().Build()
	out, layout := rewrite(t, file)
	if err := faststart.Verify(ctx, mp4test.Memory(out), mp4test.Memory(file), layout, faststart.DefaultLimits); err != nil {
		t.Fatalf("the rewrite: %v", err)
	}
	moovChanged := bytes.Clone(out)
	moovChanged[layout.MoovAt+int64(bytes.Index(out[layout.MoovAt:], []byte("stco")))+12]++
	shifted := bytes.Clone(out)
	mediaFrom := layout.MoovAt + int64(bytes.Index(out[layout.MoovAt:], []byte("mdat"))) + 4
	copy(shifted[mediaFrom:], append([]byte{0}, out[mediaFrom:len(out)-1]...))
	for name, stored := range map[string][]byte{
		"the source":         file,
		"a moov changed":     moovChanged,
		"media out of place": shifted,
		"cut short":          out[:len(out)-8],
	} {
		stored := stored
		l := layout
		if int64(len(stored)) != l.Size {
			// The size alone tells; a stored copy of another size is
			// refused before the walk. Here, the walk still must not take
			// it.
			l.Size = int64(len(stored))
		}
		if err := faststart.Verify(ctx, mp4test.Memory(stored), mp4test.Memory(file), l, faststart.DefaultLimits); !errors.Is(err, faststart.ErrMismatch) {
			t.Errorf("%s: %v, want ErrMismatch", name, err)
		}
	}
}

// Storage copies a run of the rewrite itself only when it is one run of
// the source's bytes: never across the rewritten moov or from one run into
// the next.
func TestCopySourceIsOneRunOfTheSource(t *testing.T) {
	t.Parallel()
	m := mp4test.TwoTracks()
	m.After = [][]byte{mp4test.Box("free", make([]byte, 40))}
	file, _ := m.Build()
	_, layout := rewrite(t, file)
	// The source: ftyp, mdat, moov, then the 48-byte free box.
	oldMoovAt := int64(bytes.Index(file, []byte("moov")) - 4)
	moovLen := int64(len(file)) - 48 - oldMoovAt
	mediaAt := layout.MoovAt + moovLen
	mediaLen := oldMoovAt - layout.MoovAt
	for name, c := range map[string]struct {
		off, n int64
		from   int64
		ok     bool
	}{
		"inside the media data":     {mediaAt + 8, 20, layout.MoovAt + 8, true},
		"the whole media data":      {mediaAt, mediaLen, layout.MoovAt, true},
		"across the moov":           {layout.MoovAt - 4, 20, 0, false},
		"inside the moov":           {layout.MoovAt + 8, 8, 0, false},
		"from the media data on":    {mediaAt, mediaLen + 1, 0, false},
		"inside what followed moov": {layout.Size - 40, 40, int64(len(file)) - 40, true},
	} {
		from, ok := layout.CopySource(c.off, c.n)
		if ok != c.ok || (ok && from != c.from) {
			t.Errorf("%s: from %d ok %v, want %d %v", name, from, ok, c.from, c.ok)
		}
	}
}
