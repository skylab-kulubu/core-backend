package certificate

import (
	"context"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/media"
)

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
	data, err := m.Blobs.Read(ctx, metadata.Key)
	if err != nil {
		return Asset{}, err
	}
	return Asset{ContentType: metadata.Type, Data: data}, nil
}
