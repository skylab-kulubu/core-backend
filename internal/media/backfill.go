package media

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

const (
	coverColorBackfillBatchSize = 25
	backfillBatchSize           = 25
)

func BackfillCoverColors(ctx context.Context, store Store, blobs BlobStore) (int, bool, error) {
	items, err := store.ListPendingCoverColors(ctx, coverColorBackfillBatchSize)
	if err != nil {
		return 0, false, err
	}
	for i, item := range items {
		data, err := blobs.Read(ctx, item.Key)
		if err != nil {
			return i, false, err
		}
		if err := store.SetCoverColors(ctx, item.ID, ExtractCoverColors(data)); err != nil {
			return i, false, err
		}
	}
	return len(items), len(items) < coverColorBackfillBatchSize, nil
}

func MaintainCoverColorBackfill(ctx context.Context, store Store, blobs BlobStore, retryEvery time.Duration, onError func(error)) {
	go func() {
		for {
			_, done, err := BackfillCoverColors(ctx, store, blobs)
			if err == nil {
				if done {
					return
				}
				continue
			}
			if onError != nil {
				onError(err)
			}
			timer := time.NewTimer(retryEvery)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		}
	}()
}

// BackfillReport counts one pass of a backfill.
type BackfillReport struct {
	Applied int
	Failed  int
}

// BackfillServingPolicy makes one pass over the media stored before the
// serving policy. Upload wrote those objects with the recorded type as their
// only metadata, so an object is rewritten in place only when the policy
// serves it differently; the record keeps its type and is only flagged. An
// object that is already gone has nothing left to serve and counts as done.
func BackfillServingPolicy(ctx context.Context, store Store, blobs BlobStore, onError func(error)) (BackfillReport, error) {
	return BackfillPass(ctx, "media", store.ListPendingServingPolicy,
		func(item Media) uuid.UUID { return item.ID },
		func(ctx context.Context, item Media) error { return applyServingPolicy(ctx, store, blobs, item) },
		onError)
}

// BackfillError is an item a backfill pass could not apply, named by kind
// and id.
type BackfillError struct {
	Kind string
	ID   uuid.UUID
	Err  error
}

func (e *BackfillError) Error() string { return fmt.Sprintf("%s %s: %v", e.Kind, e.ID, e.Err) }

func (e *BackfillError) Unwrap() error { return e.Err }

// BackfillPass walks every pending item once, by id in batches of 25, and
// applies each one. An item that fails is reported through onError as a
// *BackfillError and left pending for the next pass, so it never holds up
// the items after it. A cancelled ctx ends the pass with its error and the
// counts so far. apply must be idempotent: a pass cut short is run again.
func BackfillPass[T any](ctx context.Context, kind string, list func(ctx context.Context, after uuid.UUID, limit int) ([]T, error), id func(T) uuid.UUID, apply func(context.Context, T) error, onError func(error)) (BackfillReport, error) {
	var report BackfillReport
	after := uuid.Nil
	for {
		items, err := list(ctx, after, backfillBatchSize)
		if err != nil {
			return report, err
		}
		for _, item := range items {
			if err := ctx.Err(); err != nil {
				return report, err
			}
			if err := apply(ctx, item); err != nil {
				report.Failed++
				if onError != nil {
					onError(&BackfillError{Kind: kind, ID: id(item), Err: err})
				}
				continue
			}
			report.Applied++
		}
		if len(items) < backfillBatchSize {
			return report, nil
		}
		after = id(items[len(items)-1])
	}
}

func applyServingPolicy(ctx context.Context, store Store, blobs BlobStore, item Media) error {
	serving := ServingMetadata(item.Type, item.Name)
	if serving != (BlobMetadata{ContentType: item.Type}) {
		if err := blobs.SetMetadata(ctx, item.Key, serving); err != nil && !errors.Is(err, ErrNotFound) {
			return err
		}
	}
	return store.SetServingPolicyApplied(ctx, item.ID)
}

// MaintainServingPolicyBackfill runs BackfillServingPolicy in the background
// until a pass leaves nothing failed.
func MaintainServingPolicyBackfill(ctx context.Context, store Store, blobs BlobStore, retryEvery time.Duration, onError func(error)) {
	MaintainBackfill(ctx, func(ctx context.Context) (BackfillReport, error) {
		return BackfillServingPolicy(ctx, store, blobs, onError)
	}, retryEvery, onError)
}

// MaintainBackfill runs pass in the background until one ends with nothing
// failed, waiting retryEvery after a pass that left failures or could not
// finish.
func MaintainBackfill(ctx context.Context, pass func(context.Context) (BackfillReport, error), retryEvery time.Duration, onError func(error)) {
	go func() {
		for {
			report, err := pass(ctx)
			if err == nil && report.Failed == 0 {
				return
			}
			if err != nil && onError != nil {
				onError(err)
			}
			timer := time.NewTimer(retryEvery)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		}
	}()
}
