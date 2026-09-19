package media

import (
	"context"
	"time"
)

const coverColorBackfillBatchSize = 25

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
