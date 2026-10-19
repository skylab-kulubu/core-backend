package media_test

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/media"
)

// Stopping a background backfill is not a failure: a pass cut short by the
// cancelled context is not reported through onError.
func TestMaintainBackfillReportsNothingWhenStopped(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		var reported []error
		media.MaintainBackfill(ctx, func(ctx context.Context) (media.BackfillReport, error) {
			<-ctx.Done()
			return media.BackfillReport{}, ctx.Err()
		}, time.Minute, func(err error) { reported = append(reported, err) })
		synctest.Wait()

		cancel()
		synctest.Wait()

		if len(reported) != 0 {
			t.Fatalf("reported %v after the stop, want nothing", reported)
		}
	})
}

// An item whose apply fails because the pass was stopped meanwhile is neither
// counted as failed nor reported: the pass ends with the context's error.
func TestBackfillPassDoesNotReportAnItemCutShortByTheStop(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	items := []uuid.UUID{uuid.New(), uuid.New(), uuid.New()}
	applied := 0
	report, err := media.BackfillPass(ctx, "media",
		func(context.Context, uuid.UUID, int) ([]uuid.UUID, error) { return items, nil },
		func(id uuid.UUID) uuid.UUID { return id },
		func(ctx context.Context, _ uuid.UUID) error {
			applied++
			if applied == 2 {
				cancel()
				return ctx.Err()
			}
			return nil
		}, func(err error) { t.Errorf("failure reported: %v", err) })
	if !errors.Is(err, context.Canceled) || report != (media.BackfillReport{Applied: 1}) {
		t.Fatalf("report %+v err %v", report, err)
	}
}
