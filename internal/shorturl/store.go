package shorturl

import (
	"context"
	"time"

	"github.com/google/uuid"
)

type Store interface {
	Create(ctx context.Context, u URL) (URL, error)
	Get(ctx context.Context, id uuid.UUID) (URL, error)
	GetByAlias(ctx context.Context, alias string) (URL, error)
	ListByCreator(ctx context.Context, userID uuid.UUID) ([]URL, error)
	ListAll(ctx context.Context) ([]URL, error)
	Update(ctx context.Context, u URL) (URL, error)
	Delete(ctx context.Context, id uuid.UUID) error
	RecordHit(ctx context.Context, id uuid.UUID, hit Hit) (URL, error)
	ListHits(ctx context.Context, id uuid.UUID, since time.Time) ([]Hit, error)
}
