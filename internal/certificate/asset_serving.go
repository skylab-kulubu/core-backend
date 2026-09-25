package certificate

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/media"
)

// AssetServingStore finds the published template versions whose asset
// copies were stored before the media serving policy.
type AssetServingStore interface {
	// ListPendingAssetServingPolicy returns, in id order and after the given
	// id, the versions not yet flagged, with their asset manifests.
	ListPendingAssetServingPolicy(ctx context.Context, after uuid.UUID, limit int) ([]TemplateVersion, error)
	SetAssetServingPolicyApplied(ctx context.Context, id uuid.UUID) error
}

// BackfillAssetServingPolicy makes one pass over the template versions
// published before the media serving policy. Their asset copies in
// certificate-template-assets/ carry the asset's type as their only
// metadata, so a copy is rewritten in place only when the policy serves it
// differently. A copy that is already gone has nothing left to serve.
func BackfillAssetServingPolicy(ctx context.Context, store AssetServingStore, blobs media.BlobStore, onError func(error)) (media.BackfillReport, error) {
	return media.BackfillPass(ctx, "certificate template version", store.ListPendingAssetServingPolicy,
		func(version TemplateVersion) uuid.UUID { return version.ID },
		func(ctx context.Context, version TemplateVersion) error {
			return applyAssetServingPolicy(ctx, store, blobs, version)
		},
		onError)
}

func applyAssetServingPolicy(ctx context.Context, store AssetServingStore, blobs media.BlobStore, version TemplateVersion) error {
	for _, ref := range version.AssetManifest {
		serving := media.ServingMetadata(ref.ContentType, "")
		if serving == (media.BlobMetadata{ContentType: ref.ContentType}) {
			continue
		}
		if err := blobs.SetMetadata(ctx, ref.Key, serving); err != nil && !errors.Is(err, media.ErrNotFound) {
			return err
		}
	}
	return store.SetAssetServingPolicyApplied(ctx, version.ID)
}

// MaintainAssetServingPolicyBackfill runs BackfillAssetServingPolicy in the
// background until a pass leaves nothing failed.
func MaintainAssetServingPolicyBackfill(ctx context.Context, store AssetServingStore, blobs media.BlobStore, retryEvery time.Duration, onError func(error)) {
	media.MaintainBackfill(ctx, func(ctx context.Context) (media.BackfillReport, error) {
		return BackfillAssetServingPolicy(ctx, store, blobs, onError)
	}, retryEvery, onError)
}
