package certificate

import (
	"context"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/media"
)

const maxTemplateAssetBytes = 50 << 20

type versionAssetReader struct {
	manifest  map[string]VersionAssetRef
	artifacts ArtifactStore
}

func (r versionAssetReader) ReadAsset(ctx context.Context, id uuid.UUID) (Asset, error) {
	ref, ok := r.manifest[id.String()]
	if !ok || ref.Key == "" || ref.ContentType == "" || r.artifacts == nil {
		return Asset{}, ErrNotFound
	}
	if sealed, private := ref.Sealed(); private {
		return Asset{ContentType: ref.ContentType, Sealed: &sealed}, nil
	}
	data, err := r.artifacts.Read(ctx, ref.Key)
	if err != nil {
		return Asset{}, err
	}
	return Asset{ContentType: ref.ContentType, Data: data}, nil
}

func layoutAssetIDs(layout Layout) []uuid.UUID {
	seen := make(map[uuid.UUID]struct{})
	if layout.BackgroundMediaID != nil {
		seen[*layout.BackgroundMediaID] = struct{}{}
	}
	for _, element := range layout.Elements {
		if element.MediaID != nil {
			seen[*element.MediaID] = struct{}{}
		}
	}
	out := make([]uuid.UUID, 0, len(seen))
	for id := range seen {
		out = append(out, id)
	}
	return out
}

func (s *service) snapshotVersionAssets(ctx context.Context, versionID uuid.UUID, layout Layout) (manifest map[string]VersionAssetRef, err error) {
	ids := layoutAssetIDs(layout)
	manifest = make(map[string]VersionAssetRef, len(ids))
	if len(ids) == 0 {
		return manifest, nil
	}
	if s.assets == nil || s.artifacts == nil {
		return nil, ErrInvalid
	}
	defer func() {
		if err != nil {
			s.discardPrivateCopies(ctx, manifest)
		}
	}()
	assets := s.decrypted(s.assets)
	total := 0
	for _, id := range ids {
		asset, err := assets.ReadAsset(ctx, id)
		if err != nil {
			return nil, err
		}
		total += len(asset.Data)
		if len(asset.Data) == 0 || total > maxTemplateAssetBytes {
			return nil, ErrInvalid
		}
		// The version id is new on every publish, so no two attempts share
		// a copy's key.
		key := "certificate-template-assets/" + versionID.String() + "/" + id.String()
		if asset.Sealed != nil {
			// The copy of a private asset stays encrypted, in the private
			// bucket, under a data key of its own.
			sealed, err := s.privateArtifacts.Seal(ctx, media.PrivateObjectKey(key), asset.Data)
			if err != nil {
				return nil, err
			}
			manifest[id.String()] = VersionAssetRef{Key: sealed.Key, ContentType: asset.ContentType, Encryption: &sealed.Encryption}
			continue
		}
		// The copy is as public as the Media it came from, so it is served
		// under the same policy; the manifest keeps the type for rendering.
		if err := s.artifacts.Put(ctx, key, asset.Data, media.ServingMetadata(asset.ContentType, "")); err != nil {
			return nil, err
		}
		manifest[id.String()] = VersionAssetRef{Key: key, ContentType: asset.ContentType}
	}
	return manifest, nil
}

// discardPrivateCopies deletes the private copies a publish that failed made
// (its own, under its new version id). A copy that cannot be deleted is left
// as ciphertext nothing refers to.
func (s *service) discardPrivateCopies(ctx context.Context, manifest map[string]VersionAssetRef) {
	for _, ref := range manifest {
		if ref.Encryption != nil && s.privateArtifacts != nil {
			_ = s.privateArtifacts.Delete(context.WithoutCancel(ctx), ref.Key)
		}
	}
}

// decrypted reads assets through reader, decrypting the private ones.
func (s *service) decrypted(reader AssetReader) AssetReader {
	return decryptingAssets{reader: reader, private: s.privateArtifacts}
}

func (s *service) assetsForVersion(version TemplateVersion) AssetReader {
	if len(version.AssetManifest) == 0 {
		return s.decrypted(s.assets)
	}
	return s.decrypted(versionAssetReader{manifest: version.AssetManifest, artifacts: s.artifacts})
}
