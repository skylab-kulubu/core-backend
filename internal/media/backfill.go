package media

import (
	"context"
	"errors"
	"time"
)

const (
	coverColorBackfillBatchSize    = 25
	servingPolicyBackfillBatchSize = 25
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

// BackfillServingPolicy brings one batch of objects stored before the serving
// policy under it. Upload wrote those with the recorded type as their only
// metadata, so an object is rewritten in place only when the policy serves
// it differently; the record then reports the type the CDN serves. An object
// that is already gone has nothing left to serve and is recorded as done.
// Every step is idempotent, so a batch cut short is simply taken again.
func BackfillServingPolicy(ctx context.Context, store Store, blobs BlobStore) (int, bool, error) {
	items, err := store.ListPendingServingPolicy(ctx, servingPolicyBackfillBatchSize)
	if err != nil {
		return 0, false, err
	}
	for i, item := range items {
		serving := servingMetadata(item.Type, item.Name)
		if serving != (BlobMetadata{ContentType: item.Type}) {
			if err := blobs.SetMetadata(ctx, item.Key, serving); err != nil && !errors.Is(err, ErrNotFound) {
				return i, false, err
			}
		}
		if err := store.SetServingPolicyApplied(ctx, item.ID, serving.ContentType); err != nil {
			return i, false, err
		}
	}
	return len(items), len(items) < servingPolicyBackfillBatchSize, nil
}

func MaintainCoverColorBackfill(ctx context.Context, store Store, blobs BlobStore, retryEvery time.Duration, onError func(error)) {
	maintainBackfill(ctx, store, blobs, BackfillCoverColors, retryEvery, onError)
}

// MaintainServingPolicyBackfill runs BackfillServingPolicy in the background,
// batch after batch until nothing is pending, and retries after a failure.
func MaintainServingPolicyBackfill(ctx context.Context, store Store, blobs BlobStore, retryEvery time.Duration, onError func(error)) {
	maintainBackfill(ctx, store, blobs, BackfillServingPolicy, retryEvery, onError)
}

func maintainBackfill(ctx context.Context, store Store, blobs BlobStore, backfill func(context.Context, Store, BlobStore) (int, bool, error), retryEvery time.Duration, onError func(error)) {
	go func() {
		for {
			_, done, err := backfill(ctx, store, blobs)
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
