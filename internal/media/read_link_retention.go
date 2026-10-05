package media

import (
	"context"
	"log"
	"time"
)

// ReadLinkRetention is how long the access log of private Media keeps a read
// link and its opens (decision G2): one year, whoever the link named, erased
// accounts included, as an access audit record. With the retention sweep in
// apply mode the record is kept three years and only the open's address goes
// after one (ADR-0062, retention.ReadLinkWindow).
const ReadLinkRetention = 365 * 24 * time.Hour

// ReadLinkPruner deletes the access log rows past their retention
// (PostgresStore).
type ReadLinkPruner interface {
	// PruneReadLinks deletes the read links issued before the cutoff, with
	// their opens, and any open made before it, and reports how many links
	// went. It is idempotent.
	PruneReadLinks(ctx context.Context, before time.Time) (int64, error)
}

// MaintainReadLinkRetention prunes the access log in the background, first
// at once and then on every interval, so a large first run never holds up
// startup: the links issued, and the opens made, more than window ago go. A
// failed run is reported and the next one tries again; a run that deletes
// nothing says nothing.
func MaintainReadLinkRetention(ctx context.Context, pruner ReadLinkPruner, interval, window time.Duration, onError func(error)) {
	prune := func() {
		deleted, err := pruner.PruneReadLinks(ctx, time.Now().UTC().Add(-window))
		if err != nil {
			if onError != nil {
				onError(err)
			}
			return
		}
		if deleted > 0 {
			log.Printf("media read link retention: deleted %d read links (and their opens) older than %s", deleted, window)
		}
	}
	go func() {
		prune()
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
