package media

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
)

// BackfillImageSizes makes one pass over the image Media stored before
// core made image sizes, and makes theirs: each size of the Media's purpose
// the image is larger than is stored beside it at <key>/<size>, upright,
// and recorded with the image's size. The original is never rewritten:
// legacy originals keep their bytes, and purposed ones were re-encoded on
// upload. An image core cannot read (its object is gone, it does not decode,
// it is not a raster image) is recorded with no sizes, so no pass tries it
// again. A failure to read or write an object leaves the Media for the next
// pass and never holds up the others (BackfillPass).
func BackfillImageSizes(ctx context.Context, store Store, blobs BlobStore, catalogue Catalogue, onError func(error)) (BackfillReport, error) {
	return BackfillPass(ctx, "media", store.ListPendingImageSizes,
		func(item Media) uuid.UUID { return item.ID },
		func(ctx context.Context, item Media) error {
			return makeImageSizes(ctx, store, blobs, catalogue, item)
		},
		onError)
}

func makeImageSizes(ctx context.Context, store Store, blobs BlobStore, catalogue Catalogue, item Media) error {
	if !isRasterType(item.Type) || isAbsoluteURL(item.Key) {
		return recordImageSizes(ctx, store, item.ID, ImageSize{}, nil)
	}
	data, err := blobs.Read(ctx, item.Key)
	if errors.Is(err, ErrNotFound) {
		return recordImageSizes(ctx, store, item.ID, ImageSize{}, nil)
	}
	if err != nil {
		return err
	}
	purpose, _ := catalogue.Lookup(item.Purpose)
	release, err := acquireImageWork(ctx)
	if err != nil {
		return err
	}
	kept := keptImageSizes(data, purpose.Image.Sizes)
	release()
	written := make(map[string]ImageSize, len(kept.variants))
	for _, variant := range kept.variants {
		if err := blobs.Put(ctx, sizeObjectKey(item.Key, variant.size), variant.body, ServingMetadata(variant.ctype, "")); err != nil {
			return err
		}
		written[variant.size] = ImageSize{Width: variant.width, Height: variant.height}
	}
	err = store.SetImageSizes(ctx, item.ID, kept.size, written)
	if errors.Is(err, ErrPurgeInProgress) || errors.Is(err, ErrPurged) {
		// The image is being purged: the purge may already have run past
		// the sizes just written, so they go now.
		return purgeObjects(item.Key, func(key string) error {
			if key == item.Key {
				return nil
			}
			return blobs.Delete(ctx, key)
		})
	}
	return err
}

func recordImageSizes(ctx context.Context, store Store, id uuid.UUID, size ImageSize, variants map[string]ImageSize) error {
	err := store.SetImageSizes(ctx, id, size, variants)
	if errors.Is(err, ErrPurgeInProgress) || errors.Is(err, ErrPurged) {
		return nil
	}
	return err
}

// MaintainImageSizeBackfill runs BackfillImageSizes in the background
// until a pass leaves nothing failed.
func MaintainImageSizeBackfill(ctx context.Context, store Store, blobs BlobStore, catalogue Catalogue, retryEvery time.Duration, onError func(error)) {
	MaintainBackfill(ctx, func(ctx context.Context) (BackfillReport, error) {
		return BackfillImageSizes(ctx, store, blobs, catalogue, onError)
	}, retryEvery, onError)
}
