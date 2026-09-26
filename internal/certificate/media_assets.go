package certificate

import (
	"context"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/media"
)

// MediaAssets reads a layout's assets from their Media: a public one from
// the public bucket, a private one as its sealed object, which the service
// decrypts (decryptingAssets). Only core's own private purpose is read: a
// certificate asset.
type MediaAssets struct {
	Media media.Store
	Blobs media.BlobStore
}

func (m MediaAssets) ReadAsset(ctx context.Context, id uuid.UUID) (Asset, error) {
	if m.Media == nil || m.Blobs == nil {
		return Asset{}, ErrInvalid
	}
	metadata, err := m.Media.Get(ctx, id)
	if err != nil {
		return Asset{}, err
	}
	if sealed, private := metadata.Sealed(); private {
		// A second line behind the link rule: another product's private
		// Media (an Answer file) is never decrypted into a certificate.
		if metadata.Purpose != media.PurposeCertificateAsset {
			return Asset{}, ErrNotFound
		}
		return Asset{ContentType: metadata.Type, Sealed: &sealed}, nil
	}
	data, err := m.Blobs.Read(ctx, metadata.Key)
	if err != nil {
		return Asset{}, err
	}
	return Asset{ContentType: metadata.Type, Data: data}, nil
}

// decryptingAssets reads assets through reader and decrypts the private
// ones with the private bucket.
type decryptingAssets struct {
	reader  AssetReader
	private media.PrivateObjects
}

func (d decryptingAssets) ReadAsset(ctx context.Context, id uuid.UUID) (Asset, error) {
	asset, err := d.reader.ReadAsset(ctx, id)
	if err != nil || asset.Sealed == nil {
		return asset, err
	}
	if d.private == nil {
		return Asset{}, media.ErrPrivateMediaDisabled
	}
	asset.Data, err = d.private.Read(ctx, *asset.Sealed)
	if err != nil {
		return Asset{}, err
	}
	return asset, nil
}
