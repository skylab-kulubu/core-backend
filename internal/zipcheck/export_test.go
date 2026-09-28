package zipcheck

import (
	"context"
	"time"
)

// CheckWithClock is Check started at start, reading the time from now.
func CheckWithClock(ctx context.Context, src Source, size int64, limits Limits, start time.Time, now func() time.Time) error {
	return check(ctx, src, size, limits, clock{start: start, now: now})
}
