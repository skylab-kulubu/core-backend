package identity

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

type Service interface {
	ListGroups(ctx context.Context, p authz.Principal) ([]Group, error)
	Members(ctx context.Context, p authz.Principal, groupRef string) ([]Person, error)
	AddMember(ctx context.Context, p authz.Principal, groupRef string, userID uuid.UUID) error
	RemoveMember(ctx context.Context, p authz.Principal, groupRef string, userID uuid.UUID) error
	CreateUser(ctx context.Context, p authz.Principal, in Person) (Person, error)
	DeleteUser(ctx context.Context, p authz.Principal, id uuid.UUID) error
}

type service struct {
	dir   Directory
	users user.Store
	authz authz.Authorizer
}

func NewService(dir Directory, users user.Store, az authz.Authorizer) Service {
	return &service{dir: dir, users: users, authz: az}
}

func (s *service) allow(p authz.Principal, t authz.Type, a authz.Action) error {
	if !s.authz.Allow(p, authz.Resource{Type: t}, a) {
		return ErrForbidden
	}
	return nil
}

func (s *service) ListGroups(ctx context.Context, p authz.Principal) ([]Group, error) {
	if err := s.allow(p, authz.TypeGroup, authz.Read); err != nil {
		return nil, err
	}
	return s.dir.ListGroups(ctx)
}

func (s *service) Members(ctx context.Context, p authz.Principal, groupRef string) ([]Person, error) {
	if err := s.allow(p, authz.TypeGroup, authz.Read); err != nil {
		return nil, err
	}
	return s.dir.Members(ctx, groupRef)
}

func (s *service) AddMember(ctx context.Context, p authz.Principal, groupRef string, userID uuid.UUID) error {
	if err := s.allow(p, authz.TypeGroup, authz.Update); err != nil {
		return err
	}
	return s.dir.AddMember(ctx, groupRef, userID)
}

func (s *service) RemoveMember(ctx context.Context, p authz.Principal, groupRef string, userID uuid.UUID) error {
	if err := s.allow(p, authz.TypeGroup, authz.Update); err != nil {
		return err
	}
	return s.dir.RemoveMember(ctx, groupRef, userID)
}

func (s *service) CreateUser(ctx context.Context, p authz.Principal, in Person) (Person, error) {
	if err := s.allow(p, authz.TypeUser, authz.Create); err != nil {
		return Person{}, err
	}
	if in.Email == "" {
		return Person{}, ErrInvalid
	}
	created, err := s.dir.CreateUser(ctx, in)
	if err != nil {
		return Person{}, err
	}
	_, err = s.users.Upsert(ctx, user.User{
		ID:        created.ID,
		Email:     created.Email,
		FirstName: created.FirstName,
		LastName:  created.LastName,
	})
	if err != nil {
		_ = s.dir.DeleteUser(ctx, created.ID)
		return Person{}, err
	}
	return created, nil
}

func (s *service) DeleteUser(ctx context.Context, p authz.Principal, id uuid.UUID) error {
	if err := s.allow(p, authz.TypeUser, authz.Delete); err != nil {
		return err
	}
	if err := s.dir.DeleteUser(ctx, id); err != nil {
		return err
	}
	err := s.users.Delete(ctx, id)
	if err != nil && !errors.Is(err, user.ErrNotFound) {
		return err
	}
	return nil
}
