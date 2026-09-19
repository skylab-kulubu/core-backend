package shorturl

import (
	"context"
	"time"
)

type HitPruner interface {
	PruneHits(ctx context.Context, before time.Time) error
}

func MaintainHitRetention(ctx context.Context, pruner HitPruner, interval time.Duration, report func(error)) {
	prune := func() {
		if err := pruner.PruneHits(ctx, time.Now().UTC().Add(-HitRetention)); err != nil && report != nil {
			report(err)
		}
	}
	prune()
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				prune()
			}
		}
	}()
}
