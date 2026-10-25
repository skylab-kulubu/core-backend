package events

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"
)

// RecordRetention is how long a consumed event's record is kept (ADR-0071):
// longer than a publisher's outbox (7 days) and a DLQ (14 days) can hand the
// same event over again.
const RecordRetention = 30 * 24 * time.Hour

// RetentionInterval is how often the cleanup runs.
const RetentionInterval = time.Hour

// Retention deletes the records older than RecordRetention, hourly. It runs
// whether or not the consumer is on: records left from a time it was on still
// go on time.
type Retention struct {
	store    Store
	interval time.Duration
	now      func() time.Time
	logf     func(string, ...any)

	deleted  atomic.Int64
	failures atomic.Int64
}

// NewRetention builds the cleanup; logf may be nil.
func NewRetention(store Store, logf func(string, ...any)) *Retention {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &Retention{store: store, interval: RetentionInterval, now: time.Now, logf: logf}
}

// Run cleans up at once and then every interval until ctx ends; the channel
// closes when it has stopped.
func (r *Retention) Run(ctx context.Context) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(r.interval)
		defer ticker.Stop()
		for {
			r.Once(ctx)
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	return done
}

// Once runs one cleanup.
func (r *Retention) Once(ctx context.Context) {
	n, err := r.store.DeleteBefore(ctx, r.now().Add(-RecordRetention))
	r.deleted.Add(n)
	if err != nil && ctx.Err() == nil {
		r.failures.Add(1)
		r.logf(`{"event":"events_record_cleanup_failed","level":"warn","deleted":%d,"error":%q}`, n, err.Error())
	}
}

// Prometheus is the cleanup's counters.
func (r *Retention) Prometheus() string {
	if r == nil {
		return ""
	}
	return fmt.Sprintf("# TYPE skylab_events_records_deleted_total counter\nskylab_events_records_deleted_total %d\n"+
		"# TYPE skylab_events_record_cleanup_failures_total counter\nskylab_events_record_cleanup_failures_total %d\n",
		r.deleted.Load(), r.failures.Load())
}
