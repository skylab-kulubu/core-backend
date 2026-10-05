package retention

import (
	"context"
	"errors"
	"time"
)

// CheckInterval is how often the schedule looks whether the daily run is
// due and reads the records for the metrics.
const CheckInterval = time.Hour

// runTimeout bounds one scheduled run.
const runTimeout = 30 * time.Minute

// Maintain runs the sweep in the configured mode whenever it is due (Due),
// looking first at once and then every interval, in the background, and
// refreshes the metrics after each look. A run another replica holds is not
// an error. The returned channel closes after ctx is cancelled. With the
// mode off it starts nothing.
func Maintain(ctx context.Context, sweeper *Sweeper, metrics *Metrics, interval time.Duration, onError func(error)) <-chan struct{} {
	done := make(chan struct{})
	mode := sweeper.Config().Mode
	if mode == ModeOff {
		close(done)
		return done
	}
	report := func(err error) {
		if err != nil && onError != nil {
			onError(err)
		}
	}
	go func() {
		defer close(done)
		timer := time.NewTimer(0)
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
				due, err := sweeper.Due(ctx, mode)
				report(err)
				if err == nil && due {
					runCtx, cancel := context.WithTimeout(ctx, runTimeout)
					_, err := sweeper.Run(runCtx, RunOptions{Mode: mode, Trigger: TriggerSchedule, OnlyIfDue: true})
					cancel()
					if !errors.Is(err, ErrLocked) && !errors.Is(err, ErrNotDue) {
						report(err)
					}
				}
				if metrics != nil {
					report(metrics.Refresh(ctx))
				}
				timer.Reset(interval)
			}
		}
	}()
	return done
}
