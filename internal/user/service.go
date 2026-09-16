package user

import (
	"context"

	"github.com/google/uuid"
)

type Service interface {
	Ensure(ctx context.Context, id uuid.UUID, profile Profile) (User, bool, error)
}

type service struct {
	store Store
}

func NewService(store Store) Service {
	return &service{store: store}
}

func (s *service) Ensure(ctx context.Context, id uuid.UUID, profile Profile) (User, bool, error) {
	return s.store.Upsert(ctx, User{
		ID:        id,
		Email:     profile.Email,
		FirstName: profile.FirstName,
		LastName:  profile.LastName,
	})
}
