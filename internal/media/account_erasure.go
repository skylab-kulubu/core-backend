package media

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
)

var ErrProfileBlobNotErased = errors.New("media: profile blob erasure not satisfied")

var ErrStagedUploadNotErased = errors.New("media: staged upload erasure not satisfied")

// ImmediateBlobEraser bypasses the ordinary recovery window only for profile
// media selected by the irreversible account-erasure workflow. The store still
// performs its locked reference check, so event and certificate assets remain.
type ImmediateBlobEraser struct {
	media *PostgresStore
	blobs BlobStore
}

func NewImmediateBlobEraser(media *PostgresStore, blobs BlobStore) *ImmediateBlobEraser {
	return &ImmediateBlobEraser{media: media, blobs: blobs}
}

func (e *ImmediateBlobEraser) EnsureErased(ctx context.Context, id uuid.UUID, at time.Time) error {
	purged, err := e.media.PurgeBlobIfUnreferenced(ctx, id, at, func(key string) error {
		return e.blobs.Delete(ctx, key)
	})
	if err != nil || purged {
		return err
	}
	item, getErr := e.media.GetIncludingDeleted(ctx, id)
	if getErr != nil {
		return getErr
	}
	if item.BlobPurgedAt != nil {
		return nil
	}
	// false,nil also means current/restored or newly referenced. Neither is a
	// completed erase for a deletion candidate, so retain the request linkage
	// and let the worker retry or surface manual intervention.
	return ErrProfileBlobNotErased
}

func (e *ImmediateBlobEraser) EnsureSubjectUploadsErased(ctx context.Context, subjectID uuid.UUID, at time.Time) error {
	for {
		found, err := e.media.PurgeNextSubjectStagedUpload(ctx, subjectID, at, func(key string) error {
			return e.blobs.Delete(ctx, key)
		})
		if err != nil {
			return errors.Join(ErrStagedUploadNotErased, err)
		}
		if !found {
			return nil
		}
	}
}
