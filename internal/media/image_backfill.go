package media

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// BackfillImageSizes makes one pass over the image Media stored before core
// made image sizes, and makes theirs: each size of the Media's purpose the
// image is larger than is stored beside it, upright, and recorded with the
// image's size. Only purposes whose catalogue entry names sizes are
// walked: Media uploaded without a purpose (legacy) get sizes once the
// legacy backfill (media redesign ticket 08) gives them a purpose. The
// original is never rewritten.
//
// An image core cannot read (its object is gone, it does not decode, it is
// not a raster image, its key cannot have sizes) is recorded with no
// sizes, so no pass tries it again. A failure to read or write an object
// leaves the Media for the next pass and never holds up the others
// (BackfillPass). Each image decodes within the decode budget.
func BackfillImageSizes(ctx context.Context, store Store, blobs BlobStore, catalogue Catalogue, budget *DecodeBudget, onError func(error)) (BackfillReport, error) {
	purposes := catalogue.purposesWithSizes()
	return BackfillPass(ctx, "media",
		func(ctx context.Context, after uuid.UUID, limit int) ([]Media, error) {
			return store.ListPendingImageSizes(ctx, purposes, after, limit)
		},
		func(item Media) uuid.UUID { return item.ID },
		func(ctx context.Context, item Media) error {
			return makeImageSizes(ctx, store, blobs, catalogue, budget, item)
		},
		onError)
}

// noSizes records an image made without sizes.
var noSizes = map[string]SizeObject{}

// makeImageSizes makes one image's sizes. Before it decodes, it records the
// image as done without sizes, and records the sizes over that only when
// they are stored: a decoder that panics, or a process that dies decoding
// the image (out of memory), leaves the image done without sizes, so no
// restart decodes it again.
func makeImageSizes(ctx context.Context, store Store, blobs BlobStore, catalogue Catalogue, budget *DecodeBudget, item Media) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("media: making image sizes panicked: %v", recovered)
			_ = recordImageSizes(context.WithoutCancel(ctx), store, item.ID, ImageSize{}, noSizes)
		}
	}()
	purpose, _ := catalogue.Lookup(item.Purpose)
	if !isRasterType(item.Type) || !canHaveSizeObjects(item.Key) || len(purpose.Image.Sizes) == 0 {
		return recordImageSizes(ctx, store, item.ID, ImageSize{}, noSizes)
	}
	data, err := blobs.Read(ctx, item.Key)
	if errors.Is(err, ErrNotFound) {
		return recordImageSizes(ctx, store, item.ID, ImageSize{}, noSizes)
	}
	if err != nil {
		return err
	}
	shown, sizes, err := decodeSizesOfStored(ctx, store, budget, item.ID, data, purpose.Image.Sizes)
	if err != nil || sizes == nil {
		return err
	}
	objects := sizeObjectsOf(sizes)
	for _, size := range sizes {
		if err := blobs.Put(ctx, sizeObjectKey(item.Key, size.name, size.ctype), size.body, ServingMetadata(size.ctype, "")); err != nil {
			// Not made after all: listed again on the next pass.
			return errors.Join(err, recordImageSizes(ctx, store, item.ID, ImageSize{}, nil))
		}
	}
	err = store.SetImageSizes(ctx, item.ID, shown, objects)
	if errors.Is(err, ErrPurgeInProgress) || errors.Is(err, ErrPurged) {
		// The image is being purged: the purge may already have run past
		// the sizes just written, so they go now.
		for _, size := range sizes {
			if err := blobs.Delete(ctx, sizeObjectKey(item.Key, size.name, size.ctype)); err != nil {
				return err
			}
		}
		return nil
	}
	return err
}

// decodeSizesOfStored waits for a decoding slot, records the image as done
// without sizes, and decodes it within the slot. sizes is nil when the
// image is done: it does not decode, or it is being purged.
func decodeSizesOfStored(ctx context.Context, store Store, budget *DecodeBudget, id uuid.UUID, data []byte, want map[string]int) (ImageSize, []encodedSize, error) {
	release, err := budget.Acquire(ctx)
	if err != nil {
		return ImageSize{}, nil, err
	}
	defer release()
	claimed, err := claimImageSizes(ctx, store, id)
	if err != nil || !claimed {
		return ImageSize{}, nil, err
	}
	shown, sizes, err := sizesOfStored(data, want)
	if err != nil {
		// It does not decode, or is too large to: done, without sizes.
		return ImageSize{}, nil, nil
	}
	if sizes == nil {
		sizes = []encodedSize{}
	}
	return shown, sizes, nil
}

// claimImageSizes records the image as done without sizes before anything
// decodes it. It reports false when the image's purge has begun.
func claimImageSizes(ctx context.Context, store Store, id uuid.UUID) (bool, error) {
	err := store.SetImageSizes(ctx, id, ImageSize{}, noSizes)
	if errors.Is(err, ErrPurgeInProgress) || errors.Is(err, ErrPurged) {
		return false, nil
	}
	return err == nil, err
}

func recordImageSizes(ctx context.Context, store Store, id uuid.UUID, size ImageSize, objects map[string]SizeObject) error {
	err := store.SetImageSizes(ctx, id, size, objects)
	if errors.Is(err, ErrPurgeInProgress) || errors.Is(err, ErrPurged) {
		return nil
	}
	return err
}

// MaintainImageSizeBackfill runs BackfillImageSizes in the background until
// a pass leaves nothing failed.
func MaintainImageSizeBackfill(ctx context.Context, store Store, blobs BlobStore, catalogue Catalogue, budget *DecodeBudget, retryEvery time.Duration, onError func(error)) {
	MaintainBackfill(ctx, func(ctx context.Context) (BackfillReport, error) {
		return BackfillImageSizes(ctx, store, blobs, catalogue, budget, onError)
	}, retryEvery, onError)
}
