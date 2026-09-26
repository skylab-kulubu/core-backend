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
}

func (p blockingPruner) PruneReadLinks(context.Context, time.Time) (int64, error) {
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
		media.MaintainReadLinkRetention(ctx, pruner, time.Hour, nil)
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
