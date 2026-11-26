package certificate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"

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

// placedAsset is a version's placed copy of its PDF background and the
// checksum it was stored with.
type placedAsset struct {
	Asset
	checksum string
}

// placedAssetReader reads a published version's placed copies.
type placedAssetReader interface {
	// ReadPlaced reads the version's placed copy of the PDF background id;
	// false when the version has none (published before placed copies),
	// errNotAVersion when the assets are a draft's.
	ReadPlaced(ctx context.Context, id uuid.UUID) (placedAsset, bool, error)
}

// errNotAVersion is assets that are no published version's: a draft's
// Media, whose PDF background is placed on the fly.
var errNotAVersion = errors.New("certificate: assets of no published version")

func (r versionAssetReader) ReadPlaced(ctx context.Context, id uuid.UUID) (placedAsset, bool, error) {
	ref, ok := r.manifest[id.String()]
	if !ok || ref.Placed == nil {
		return placedAsset{}, false, nil
	}
	if ref.Placed.Key == "" || r.artifacts == nil {
		return placedAsset{}, false, ErrNotFound
	}
	if sealed, private := ref.Placed.Sealed(); private {
		return placedAsset{Asset: Asset{ContentType: "application/pdf", Sealed: &sealed}, checksum: ref.Placed.Checksum}, true, nil
	}
	data, err := r.artifacts.Read(ctx, ref.Placed.Key)
	if err != nil {
		return placedAsset{}, false, err
	}
	return placedAsset{Asset: Asset{ContentType: "application/pdf", Data: data}, checksum: ref.Placed.Checksum}, true, nil
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

func (s *service) snapshotVersionAssets(ctx context.Context, versionID uuid.UUID, layout Layout) (_ map[string]VersionAssetRef, err error) {
	ids := layoutAssetIDs(layout)
	manifest := make(map[string]VersionAssetRef, len(ids))
	if len(ids) == 0 {
		return manifest, nil
	}
	if s.assets == nil || s.artifacts == nil {
		return nil, invalid(CodeAssetStorageUnavailable)
	}
	// The private copies this attempt has written, deleted if it fails.
	var sealedKeys []string
	defer func() {
		if err != nil {
			s.discardPrivateCopies(ctx, sealedKeys)
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
		if len(asset.Data) == 0 {
			return nil, invalid(CodeAssetEmpty)
		}
		if total > maxTemplateAssetBytes {
			return nil, invalid(CodeAssetTooLarge)
		}
		// A placed PDF background takes its placed copy with it, which
		// counts toward the bound too.
		var placed []byte
		if placesPDFBackground(layout, asset.ContentType) && *layout.BackgroundMediaID == id {
			if placed, err = s.placePDF(ctx, layout, asset.Data); err != nil {
				return nil, err
			}
			if total += len(placed); total > maxTemplateAssetBytes {
				return nil, invalid(CodeAssetTooLarge)
			}
		}
		// The version id is new on every publish, so no two attempts share
		// a copy's key.
		key := "certificate-template-assets/" + versionID.String() + "/" + id.String()
		ref, keys, err := s.storeVersionCopy(ctx, key, asset.ContentType, asset.Data, asset.Sealed != nil)
		sealedKeys = append(sealedKeys, keys...)
		if err != nil {
			return nil, err
		}
		if placed != nil {
			copyRef, keys, err := s.storeVersionCopy(ctx, key+".placed.pdf", "application/pdf", placed, asset.Sealed != nil)
			sealedKeys = append(sealedKeys, keys...)
			if err != nil {
				return nil, err
			}
			sum := sha256.Sum256(placed)
			ref.Placed = &PlacedAssetRef{Key: copyRef.Key, Encryption: copyRef.Encryption, Checksum: hex.EncodeToString(sum[:])}
		}
		manifest[id.String()] = ref
	}
	return manifest, nil
}

// storeVersionCopy stores one copy of a version's asset under key, and
// returns the private keys it may have written (deleted if the publish
// fails).
func (s *service) storeVersionCopy(ctx context.Context, key, contentType string, data []byte, private bool) (VersionAssetRef, []string, error) {
	if private {
		// The copy of a private asset stays encrypted, in the private
		// bucket, under a data key of its own.
		sealed, err := s.privateArtifacts.Seal(ctx, media.PrivateObjectKey(key), data)
		if err != nil {
			// A failed Put may have left the object: it is this attempt's
			// own key, so it goes too.
			return VersionAssetRef{}, []string{media.PrivateObjectKey(key)}, err
		}
		return VersionAssetRef{Key: sealed.Key, ContentType: contentType, Encryption: &sealed.Encryption}, []string{sealed.Key}, nil
	}
	// The copy is as public as the Media it came from, so it is served
	// under the same policy; the manifest keeps the type for rendering.
	if err := s.artifacts.Put(ctx, key, data, media.ServingMetadata(contentType, "")); err != nil {
		return VersionAssetRef{}, nil, err
	}
	return VersionAssetRef{Key: key, ContentType: contentType}, nil, nil
}

// discardPrivateCopies deletes private copies a publish that failed wrote:
// its own, under its new version id, which nothing refers to. A copy that
// cannot be deleted is left as ciphertext.
func (s *service) discardPrivateCopies(ctx context.Context, keys []string) {
	if s.privateArtifacts == nil {
		return
	}
	for _, key := range keys {
		_ = s.privateArtifacts.Delete(context.WithoutCancel(ctx), key)
	}
}

// privateCopyKeys are the keys of a manifest's private copies.
func privateCopyKeys(manifest map[string]VersionAssetRef) []string {
	var keys []string
	for _, ref := range manifest {
		if ref.Encryption != nil {
			keys = append(keys, ref.Key)
		}
		if ref.Placed != nil && ref.Placed.Encryption != nil {
			keys = append(keys, ref.Placed.Key)
		}
	}
	return keys
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
