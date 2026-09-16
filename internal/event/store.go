package event

import (
	"context"
	"errors"

	"github.com/google/uuid"
)

var (
	ErrNotFound  = errors.New("event: not found")
	ErrForbidden = errors.New("event: forbidden")
	ErrInvalid   = errors.New("event: invalid")
)

type Store interface {
	List(ctx context.Context, ownerTeam string) ([]Event, error)
	Get(ctx context.Context, id uuid.UUID) (Event, error)
	Create(ctx context.Context, e Event) (Event, error)
	Update(ctx context.Context, e Event) (Event, error)
	Delete(ctx context.Context, id uuid.UUID) error
}
