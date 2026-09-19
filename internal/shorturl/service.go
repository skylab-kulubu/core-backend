package shorturl

import (
	"context"
	"crypto/rand"
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
)

type Service interface {
	Redirect(ctx context.Context, alias string, hit Hit) (URL, error)
	Lookup(ctx context.Context, alias string) (URL, error)
	Create(ctx context.Context, p authz.Principal, target, alias string) (URL, error)
	ListMine(ctx context.Context, p authz.Principal) ([]URL, error)
	ListAll(ctx context.Context, p authz.Principal) ([]URL, error)
	ListHits(ctx context.Context, p authz.Principal, id uuid.UUID) ([]Hit, error)
	Update(ctx context.Context, p authz.Principal, id uuid.UUID, target, alias string) (URL, error)
	Delete(ctx context.Context, p authz.Principal, id uuid.UUID) error
}

type service struct {
	store Store
	authz authz.Authorizer
}

func NewService(store Store, az authz.Authorizer) Service {
	return &service{store: store, authz: az}
}

var aliasPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)

func (s *service) Lookup(ctx context.Context, alias string) (URL, error) {
	return s.store.GetByAlias(ctx, alias)
}

func (s *service) Redirect(ctx context.Context, alias string, hit Hit) (URL, error) {
	u, err := s.store.GetByAlias(ctx, alias)
	if err != nil {
		return URL{}, err
	}
	if hit.CreatedAt.IsZero() {
		hit.CreatedAt = time.Now().UTC()
	}
	hit.URLID = u.ID
	hit.Alias = u.Alias
	recorded, err := s.store.RecordHit(ctx, u.ID, hit)
	if err != nil {
		return u, nil
	}
	return recorded, nil
}

func (s *service) ListHits(ctx context.Context, p authz.Principal, id uuid.UUID) ([]Hit, error) {
	if _, err := s.store.Get(ctx, id); err != nil {
		return nil, err
	}
	if !s.authz.Allow(p, authz.Resource{Type: authz.TypeURL}, authz.Read) {
		return nil, ErrForbidden
	}
	return s.store.ListHits(ctx, id, time.Now().UTC().Add(-HitRetention))
}

func (s *service) Create(ctx context.Context, p authz.Principal, target, alias string) (URL, error) {
	if !s.authz.Allow(p, authz.Resource{Type: authz.TypeURL}, authz.Create) {
		return URL{}, ErrForbidden
	}
	target, err := normalizeTarget(target)
	if err != nil {
		return URL{}, err
	}
	alias, err = s.ensureAlias(ctx, alias)
	if err != nil {
		return URL{}, err
	}
	uid, err := uuid.Parse(p.ID)
	var createdBy *uuid.UUID
	if err == nil {
		createdBy = &uid
	}
	return s.store.Create(ctx, URL{Alias: alias, URL: target, CreatedBy: createdBy})
}

func (s *service) ListMine(ctx context.Context, p authz.Principal) ([]URL, error) {
	if !s.authz.Allow(p, authz.Resource{Type: authz.TypeURL}, authz.ReadMe) {
		return nil, ErrForbidden
	}
	uid, err := uuid.Parse(p.ID)
	if err != nil {
		return nil, ErrInvalid
	}
	return s.store.ListByCreator(ctx, uid)
}

func (s *service) ListAll(ctx context.Context, p authz.Principal) ([]URL, error) {
	if !s.authz.Allow(p, authz.Resource{Type: authz.TypeURL}, authz.Read) {
		return nil, ErrForbidden
	}
	return s.store.ListAll(ctx)
}

func (s *service) Update(ctx context.Context, p authz.Principal, id uuid.UUID, target, alias string) (URL, error) {
	existing, err := s.store.Get(ctx, id)
	if err != nil {
		return URL{}, err
	}
	owner := ""
	if existing.CreatedBy != nil {
		owner = existing.CreatedBy.String()
	}
	if !s.authz.Allow(p, authz.Resource{Type: authz.TypeURL, OwnerID: owner}, authz.Update) {
		return URL{}, ErrForbidden
	}
	if target != "" {
		target, err = normalizeTarget(target)
		if err != nil {
			return URL{}, err
		}
		existing.URL = target
	}
	if alias != "" && alias != existing.Alias {
		if err := validateAlias(alias); err != nil {
			return URL{}, err
		}
		existing.Alias = alias
	}
	return s.store.Update(ctx, existing)
}

func (s *service) Delete(ctx context.Context, p authz.Principal, id uuid.UUID) error {
	existing, err := s.store.Get(ctx, id)
	if err != nil {
		return err
	}
	owner := ""
	if existing.CreatedBy != nil {
		owner = existing.CreatedBy.String()
	}
	if !s.authz.Allow(p, authz.Resource{Type: authz.TypeURL, OwnerID: owner}, authz.Delete) {
		return ErrForbidden
	}
	return s.store.Delete(ctx, id)
}

func (s *service) ensureAlias(ctx context.Context, alias string) (string, error) {
	if strings.TrimSpace(alias) == "" {
		for range 8 {
			cand, err := randomAlias()
			if err != nil {
				return "", err
			}
			if _, err := s.store.GetByAlias(ctx, cand); err != nil {
				if errors.Is(err, ErrNotFound) {
					return cand, nil
				}
				return "", err
			}
		}
		return "", ErrConflict
	}
	if err := validateAlias(alias); err != nil {
		return "", err
	}
	if _, err := s.store.GetByAlias(ctx, alias); err == nil {
		return "", ErrConflict
	} else if !errors.Is(err, ErrNotFound) {
		return "", err
	}
	return alias, nil
}

func validateAlias(alias string) error {
	if !aliasPattern.MatchString(alias) {
		return ErrInvalid
	}
	switch strings.ToLower(alias) {
	case "v1", "health", "go", "urls", "api", "docs":
		return ErrInvalid
	}
	return nil
}

func normalizeTarget(raw string) (string, error) {
	u := strings.TrimSpace(raw)
	if u == "" {
		return "", ErrInvalid
	}
	if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
		u = "https://" + u
	}
	return u, nil
}

func randomAlias() (string, error) {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	out := make([]byte, 8)
	for i, b := range buf {
		out[i] = alphabet[int(b)%len(alphabet)]
	}
	return string(out), nil
}
