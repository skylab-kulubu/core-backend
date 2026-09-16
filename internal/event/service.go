package event

import (
	"context"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
)

type Service interface {
	List(ctx context.Context, ownerTeam string) ([]Event, error)
	Get(ctx context.Context, id uuid.UUID) (Event, error)
	Create(ctx context.Context, p authz.Principal, in Event) (Event, error)
	Update(ctx context.Context, p authz.Principal, id uuid.UUID, in Event) (Event, error)
	Delete(ctx context.Context, p authz.Principal, id uuid.UUID) error
}

type service struct {
	store Store
	authz authz.Authorizer
}

func NewService(store Store, az authz.Authorizer) Service {
	return &service{store: store, authz: az}
}

func resource(ownerTeam string) authz.Resource {
	return authz.Resource{Type: authz.TypeEvent, OwnerTeam: ownerTeam, EventType: ownerTeam}
}

func (s *service) List(ctx context.Context, ownerTeam string) ([]Event, error) {
	return s.store.List(ctx, ownerTeam)
}

func (s *service) Get(ctx context.Context, id uuid.UUID) (Event, error) {
	return s.store.Get(ctx, id)
}

func (s *service) Create(ctx context.Context, p authz.Principal, in Event) (Event, error) {
	if in.Name == "" || in.Location == "" || in.OwnerTeam == "" {
		return Event{}, ErrInvalid
	}
	if !s.authz.Allow(p, resource(in.OwnerTeam), authz.Create) {
		return Event{}, ErrForbidden
	}
	return s.store.Create(ctx, in)
}

func (s *service) Update(ctx context.Context, p authz.Principal, id uuid.UUID, in Event) (Event, error) {
	existing, err := s.store.Get(ctx, id)
	if err != nil {
		return Event{}, err
	}
	if !s.authz.Allow(p, resource(existing.OwnerTeam), authz.Update) {
		return Event{}, ErrForbidden
	}
	if in.Name == "" || in.Location == "" || in.OwnerTeam == "" {
		return Event{}, ErrInvalid
	}
	in.ID = existing.ID
	return s.store.Update(ctx, in)
}

func (s *service) Delete(ctx context.Context, p authz.Principal, id uuid.UUID) error {
	existing, err := s.store.Get(ctx, id)
	if err != nil {
		return err
	}
	if !s.authz.Allow(p, resource(existing.OwnerTeam), authz.Delete) {
		return ErrForbidden
	}
	return s.store.Delete(ctx, id)
}
