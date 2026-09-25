package user

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/ytu"
)

var (
	ErrNotFound                        = errors.New("user not found")
	ErrConflict                        = errors.New("user conflict")
	ErrSkyLimit                        = errors.New("sky number limit")
	ErrInvalid                         = errors.New("user invalid")
	ErrAccountBlocked                  = errors.New("user account blocked")
	ErrLeaseLost                       = errors.New("account deletion lease lost")
	ErrSelfDeletionIdempotencyConflict = errors.New("self deletion idempotency conflict")
	// ErrYTUManaged refuses an edit that would change the university,
	// faculty or department of a YTÜ-linked person: those follow the YTÜ
	// Microsoft login, which would overwrite the edit anyway.
	ErrYTUManaged = errors.New("user profile field follows the YTÜ login")
)

type Store interface {
	Get(ctx context.Context, id uuid.UUID) (User, error)
	Upsert(ctx context.Context, u User) (User, bool, error)
	// UpdateProfile writes the editable profile. For a YTÜ-linked person it
	// keeps the stored university, faculty and department whatever u holds;
	// only SetYTUProfile writes those.
	UpdateProfile(ctx context.Context, u User) (User, error)
	// SetYTUProfile stores what the YTÜ Microsoft login says and marks the
	// person YTÜ-linked. It refuses an account that is not active.
	SetYTUProfile(ctx context.Context, id uuid.UUID, p ytu.Profile) (User, error)
	Search(ctx context.Context, q string) ([]User, error)
	FindByEmail(ctx context.Context, email string) ([]User, error)
	FindByStudentCardUID(ctx context.Context, uid string) (User, error)
	SetStudentCardUID(ctx context.Context, id uuid.UUID, uid string) (User, error)
	NextSkyNumber(ctx context.Context) (string, error)
	RequestDeletion(ctx context.Context, id uuid.UUID, requestedBy *uuid.UUID) (DeletionRequest, error)
	AnonymizeAccount(ctx context.Context, id uuid.UUID, at time.Time) error
}

type SkySync interface {
	ReadSkyNumber(ctx context.Context, id uuid.UUID) (string, error)
	WriteSkyNumber(ctx context.Context, id uuid.UUID, skyNumber string) error
}
