package user

import (
	"context"
	"errors"

	"github.com/google/uuid"
)

var ErrNotFound = errors.New("user not found")

type Store interface {
	Get(ctx context.Context, id uuid.UUID) (User, error)
	Upsert(ctx context.Context, u User) (User, bool, error)
	Search(ctx context.Context, q string) ([]User, error)
	Delete(ctx context.Context, id uuid.UUID) error
}
