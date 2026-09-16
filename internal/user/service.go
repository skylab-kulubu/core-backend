package user

import (
	"context"
	"errors"

	"github.com/google/uuid"
)

type Service interface {
	Ensure(ctx context.Context, id uuid.UUID, profile Profile) (User, bool, error)
}

type service struct {
	store Store
	sync  SkySync
}

func NewService(store Store, syncs ...SkySync) Service {
	s := &service{store: store}
	if len(syncs) > 0 {
		s.sync = syncs[0]
	}
	return s
}

func (s *service) Ensure(ctx context.Context, id uuid.UUID, profile Profile) (User, bool, error) {
	if profile.SkyNumber == "" && s.sync != nil {
		if n, err := s.sync.ReadSkyNumber(ctx, id); err == nil && n != "" {
			profile.SkyNumber = n
		}
	}
	first, created, err := s.store.Upsert(ctx, User{
		ID:          id,
		Email:       profile.Email,
		FirstName:   profile.FirstName,
		LastName:    profile.LastName,
		SchoolEmail: profile.SchoolEmail,
		SkyNumber:   profile.SkyNumber,
	})
	if err != nil {
		return User{}, false, err
	}
	if first.SkyNumber != "" {
		return first, created, nil
	}
	assigned := first
	for i := 0; i < 8; i++ {
		n, err := s.store.NextSkyNumber(ctx)
		if err != nil {
			return User{}, false, err
		}
		assigned.SkyNumber = n
		got, _, err := s.store.Upsert(ctx, assigned)
		if err == nil {
			if s.sync != nil {
				_ = s.sync.WriteSkyNumber(ctx, got.ID, got.SkyNumber)
			}
			return got, created, nil
		}
		if !errors.Is(err, ErrConflict) {
			return User{}, false, err
		}
	}
	return User{}, false, ErrConflict
}
