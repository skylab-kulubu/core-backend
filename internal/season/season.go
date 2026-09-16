package season

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
)

var (
	ErrNotFound  = errors.New("season: not found")
	ErrForbidden = errors.New("season: forbidden")
	ErrInvalid   = errors.New("season: invalid")
)

type Season struct {
	ID        uuid.UUID  `json:"id"`
	Name      string     `json:"name"`
	StartDate *time.Time `json:"startDate,omitempty"`
	EndDate   *time.Time `json:"endDate,omitempty"`
	Active    bool       `json:"active"`
	CreatedAt time.Time  `json:"createdAt"`
	UpdatedAt time.Time  `json:"updatedAt"`
}

type Store interface {
	List(ctx context.Context, activeOnly bool) ([]Season, error)
	Get(ctx context.Context, id uuid.UUID) (Season, error)
	Create(ctx context.Context, s Season) (Season, error)
	Update(ctx context.Context, s Season) (Season, error)
	Delete(ctx context.Context, id uuid.UUID) error
}

type Service interface {
	List(ctx context.Context, activeOnly bool) ([]Season, error)
	Get(ctx context.Context, id uuid.UUID) (Season, error)
	Create(ctx context.Context, p authz.Principal, in Season) (Season, error)
	Update(ctx context.Context, p authz.Principal, id uuid.UUID, in Season) (Season, error)
	Delete(ctx context.Context, p authz.Principal, id uuid.UUID) error
}

type service struct {
	store Store
	authz authz.Authorizer
}

func NewService(store Store, az authz.Authorizer) Service {
	return &service{store: store, authz: az}
}

func (s *service) List(ctx context.Context, activeOnly bool) ([]Season, error) {
	return s.store.List(ctx, activeOnly)
}

func (s *service) Get(ctx context.Context, id uuid.UUID) (Season, error) {
	return s.store.Get(ctx, id)
}

func (s *service) Create(ctx context.Context, p authz.Principal, in Season) (Season, error) {
	if in.Name == "" {
		return Season{}, ErrInvalid
	}
	if !s.authz.Allow(p, authz.Resource{Type: authz.TypeSeason}, authz.Create) {
		return Season{}, ErrForbidden
	}
	return s.store.Create(ctx, in)
}

func (s *service) Update(ctx context.Context, p authz.Principal, id uuid.UUID, in Season) (Season, error) {
	if _, err := s.store.Get(ctx, id); err != nil {
		return Season{}, err
	}
	if !s.authz.Allow(p, authz.Resource{Type: authz.TypeSeason}, authz.Update) {
		return Season{}, ErrForbidden
	}
	if in.Name == "" {
		return Season{}, ErrInvalid
	}
	in.ID = id
	return s.store.Update(ctx, in)
}

func (s *service) Delete(ctx context.Context, p authz.Principal, id uuid.UUID) error {
	if _, err := s.store.Get(ctx, id); err != nil {
		return err
	}
	if !s.authz.Allow(p, authz.Resource{Type: authz.TypeSeason}, authz.Delete) {
		return ErrForbidden
	}
	return s.store.Delete(ctx, id)
}
