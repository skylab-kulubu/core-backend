package media

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
)

var (
	ErrNotFound  = errors.New("media: not found")
	ErrForbidden = errors.New("media: forbidden")
	ErrInvalid   = errors.New("media: invalid")
)

type Media struct {
	ID                  uuid.UUID `json:"id"`
	Name                string    `json:"name"`
	Type                string    `json:"type"`
	URL                 string    `json:"url"`
	Size                int64     `json:"size"`
	UploadedBy          uuid.UUID `json:"uploadedBy"`
	Kind                string    `json:"kind"`
	Key                 string    `json:"-"`
	CoverColors         []string  `json:"coverColors"`
	CoverColorsComputed bool      `json:"-"`
	CreatedAt           time.Time `json:"createdAt"`
	UpdatedAt           time.Time `json:"updatedAt"`
}

type Store interface {
	Create(ctx context.Context, m Media) (Media, error)
	Get(ctx context.Context, id uuid.UUID) (Media, error)
	List(ctx context.Context) ([]Media, error)
	ListPendingCoverColors(ctx context.Context, limit int) ([]Media, error)
	SetCoverColors(ctx context.Context, id uuid.UUID, colors []string) error
	Delete(ctx context.Context, id uuid.UUID) (Media, error)
}

type BlobStore interface {
	Put(ctx context.Context, key string, data []byte, contentType string) error
	Read(ctx context.Context, key string) ([]byte, error)
	Delete(ctx context.Context, key string) error
}
