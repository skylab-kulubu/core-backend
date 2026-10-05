package media_test

import (
	"context"
	"testing"
	"time"

	"github.com/skylab-kulubu/core-backend/internal/media"
)

// blockingPruner holds its first prune until released.
type blockingPruner struct {
	started chan struct{}
	release chan struct{}
	cutoffs chan time.Time
}

func (p blockingPruner) PruneReadLinks(_ context.Context, before time.Time) (int64, error) {
	if p.cutoffs != nil {
		p.cutoffs <- before
	}
	p.started <- struct{}{}
	<-p.release
	return 0, nil
}

// The retention cleanup never holds up startup: even its first run is in
// the background.
func TestReadLinkRetentionStartsInTheBackground(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pruner := blockingPruner{started: make(chan struct{}, 1), release: make(chan struct{})}
	defer close(pruner.release)

	returned := make(chan struct{})
	go func() {
		media.MaintainReadLinkRetention(ctx, pruner, time.Hour, media.ReadLinkRetention, nil)
		close(returned)
	}()
	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("MaintainReadLinkRetention waited for its first prune")
	}
	select {
	case <-pruner.started:
	case <-time.After(time.Second):
		t.Fatal("the first prune never ran")
	}
}

// The window is the caller's: one year, or three in the retention sweep's
// apply mode.
func TestReadLinkRetentionPrunesPastTheWindow(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pruner := blockingPruner{started: make(chan struct{}, 1), release: make(chan struct{}), cutoffs: make(chan time.Time, 1)}
	defer close(pruner.release)
	window := 3 * 365 * 24 * time.Hour
	media.MaintainReadLinkRetention(ctx, pruner, time.Hour, window, nil)
	select {
	case cutoff := <-pruner.cutoffs:
		if age := time.Since(cutoff); age < window || age > window+time.Minute {
			t.Fatalf("cutoff %s ago, want %s", age, window)
		}
	case <-time.After(time.Second):
		t.Fatal("the first prune never ran")
	}
}
