package skypass

import (
	"context"
	"errors"
	"strings"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

type Service interface {
	BindCard(ctx context.Context, p authz.Principal, uid string, target *uuid.UUID) (user.User, error)
	Lookup(ctx context.Context, p authz.Principal, uid string) (Holder, error)
	Mint(ctx context.Context, p authz.Principal) (Token, error)
	Verify(ctx context.Context, p authz.Principal, token string) (Holder, error)
	HolderFrom(ctx context.Context, token, uid string) (Holder, error)
	JWKS() JWKS
}

type service struct {
	users  user.Store
	authz  authz.Authorizer
	signer *Signer
}

func NewService(users user.Store, az authz.Authorizer, signer *Signer) Service {
	return &service{users: users, authz: az, signer: signer}
}

func (s *service) BindCard(ctx context.Context, p authz.Principal, uid string, target *uuid.UUID) (user.User, error) {
	if p.ID == "" {
		return user.User{}, ErrForbidden
	}
	normalized, err := NormalizeUID(uid)
	if err != nil {
		return user.User{}, err
	}
	actor, err := uuid.Parse(p.ID)
	if err != nil {
		return user.User{}, ErrInvalid
	}
	id := actor
	if target != nil && *target != uuid.Nil && *target != actor {
		if normalized != "" {
			return user.User{}, ErrForbidden
		}
		if !s.authz.Allow(p, authz.Resource{Type: authz.TypeUser}, authz.Update) {
			return user.User{}, ErrForbidden
		}
		id = *target
	}
	got, err := s.users.SetStudentCardUID(ctx, id, normalized)
	if errors.Is(err, user.ErrConflict) {
		return user.User{}, ErrConflict
	}
	if errors.Is(err, user.ErrNotFound) {
		return user.User{}, ErrNotFound
	}
	return got, err
}

func (s *service) Lookup(ctx context.Context, p authz.Principal, uid string) (Holder, error) {
	if !s.authz.Allow(p, authz.Resource{Type: authz.TypeTicket}, authz.Validate) {
		return Holder{}, ErrForbidden
	}
	normalized, err := NormalizeUID(uid)
	if err != nil {
		return Holder{}, err
	}
	if normalized == "" {
		return Holder{}, ErrInvalid
	}
	u, err := s.users.FindByStudentCardUID(ctx, normalized)
	if errors.Is(err, user.ErrNotFound) {
		return Holder{}, ErrNotFound
	}
	if err != nil {
		return Holder{}, err
	}
	return holder(u), nil
}

func (s *service) Mint(ctx context.Context, p authz.Principal) (Token, error) {
	if p.ID == "" {
		return Token{}, ErrForbidden
	}
	id, err := uuid.Parse(p.ID)
	if err != nil {
		return Token{}, ErrInvalid
	}
	u, err := s.users.Get(ctx, id)
	if errors.Is(err, user.ErrNotFound) {
		return Token{}, ErrNotFound
	}
	if err != nil {
		return Token{}, err
	}
	return s.signer.Mint(u)
}

func (s *service) Verify(ctx context.Context, p authz.Principal, token string) (Holder, error) {
	if !s.authz.Allow(p, authz.Resource{Type: authz.TypeTicket}, authz.Validate) {
		return Holder{}, ErrForbidden
	}
	claims, err := s.signer.Verify(token)
	if err != nil {
		return Holder{}, err
	}
	id, err := uuid.Parse(claims.Subject)
	if err != nil {
		return Holder{}, ErrInvalid
	}
	u, err := s.users.Get(ctx, id)
	if errors.Is(err, user.ErrNotFound) {
		return Holder{}, ErrNotFound
	}
	if err != nil {
		return Holder{}, err
	}
	return holder(u), nil
}

func (s *service) HolderFrom(ctx context.Context, token, uid string) (Holder, error) {
	token = strings.TrimSpace(token)
	uid = strings.TrimSpace(uid)
	if token != "" {
		claims, err := s.signer.Verify(token)
		if err != nil {
			return Holder{}, err
		}
		id, err := uuid.Parse(claims.Subject)
		if err != nil {
			return Holder{}, ErrInvalid
		}
		u, err := s.users.Get(ctx, id)
		if errors.Is(err, user.ErrNotFound) {
			return Holder{}, ErrNotFound
		}
		if err != nil {
			return Holder{}, err
		}
		return holder(u), nil
	}
	normalized, err := NormalizeUID(uid)
	if err != nil {
		return Holder{}, err
	}
	if normalized == "" {
		return Holder{}, ErrInvalid
	}
	u, err := s.users.FindByStudentCardUID(ctx, normalized)
	if errors.Is(err, user.ErrNotFound) {
		return Holder{}, ErrNotFound
	}
	if err != nil {
		return Holder{}, err
	}
	return holder(u), nil
}

func (s *service) JWKS() JWKS {
	return s.signer.JWKS()
}

func holder(u user.User) Holder {
	return Holder{
		ID:        u.ID,
		SkyNumber: u.SkyNumber,
		FirstName: u.FirstName,
		LastName:  u.LastName,
	}
}
