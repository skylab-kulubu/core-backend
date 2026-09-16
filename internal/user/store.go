package user

import (
	"context"
	"errors"

	"github.com/google/uuid"
)

var (
	ErrNotFound = errors.New("user not found")
	ErrConflict = errors.New("user conflict")
	ErrSkyLimit = errors.New("sky number limit")
	ErrInvalid  = errors.New("user invalid")
)

type Store interface {
	Get(ctx context.Context, id uuid.UUID) (User, error)
	Upsert(ctx context.Context, u User) (User, bool, error)
	UpdateProfile(ctx context.Context, u User) (User, error)
	Search(ctx context.Context, q string) ([]User, error)
	FindByEmail(ctx context.Context, email string) ([]User, error)
	NextSkyNumber(ctx context.Context) (string, error)
	Delete(ctx context.Context, id uuid.UUID) error
}

type SkySync interface {
	ReadSkyNumber(ctx context.Context, id uuid.UUID) (string, error)
	WriteSkyNumber(ctx context.Context, id uuid.UUID, skyNumber string) error
}
