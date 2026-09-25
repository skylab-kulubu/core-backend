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
	"github.com/skylab-kulubu/core-backend/internal/lifecycle"
)

type Service interface {
	Redirect(ctx context.Context, alias string, hit Hit) (URL, error)
	Lookup(ctx context.Context, alias string) (URL, error)
	Create(ctx context.Context, p authz.Principal, target, alias string) (URL, error)
	ListMine(ctx context.Context, p authz.Principal) ([]URL, error)
	ListMineLifecycle(ctx context.Context, p authz.Principal, visibility lifecycle.Visibility) ([]URL, error)
	ListAll(ctx context.Context, p authz.Principal) ([]URL, error)
	ListAllLifecycle(ctx context.Context, p authz.Principal, visibility lifecycle.Visibility) ([]URL, error)
	ListHits(ctx context.Context, p authz.Principal, id uuid.UUID) ([]Hit, error)
	Update(ctx context.Context, p authz.Principal, id uuid.UUID, target, alias string) (URL, error)
	Delete(ctx context.Context, p authz.Principal, id uuid.UUID) error
	Restore(ctx context.Context, p authz.Principal, id uuid.UUID) (URL, error)
}

type service struct {
	store Store
	authz authz.Authorizer
}

func NewService(store Store, az authz.Authorizer) Service {
	return &service{store: store, authz: az}
}

var aliasPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)

// Lookup resolves an alias, including one the link was renamed away from: a
// retired alias stays reserved for its link and keeps pointing at it, so a
// printed QR code or an old post survives a rename.
func (s *service) Lookup(ctx context.Context, alias string) (URL, error) {
	u, err := s.store.GetByAlias(ctx, alias)
	if errors.Is(err, ErrNotFound) {
		return s.store.GetByRetiredAlias(ctx, alias)
	}
	return u, err
}

func (s *service) Redirect(ctx context.Context, alias string, hit Hit) (URL, error) {
	u, err := s.Lookup(ctx, alias)
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
	return s.ListMineLifecycle(ctx, p, lifecycle.CurrentOnly)
}

func (s *service) ListMineLifecycle(ctx context.Context, p authz.Principal, visibility lifecycle.Visibility) ([]URL, error) {
	if !s.authz.Allow(p, authz.Resource{Type: authz.TypeURL}, authz.ReadMe) {
		return nil, ErrForbidden
	}
	uid, err := uuid.Parse(p.ID)
	if err != nil {
		return nil, ErrInvalid
	}
	return s.store.ListByCreatorLifecycle(ctx, uid, visibility)
}

func (s *service) ListAll(ctx context.Context, p authz.Principal) ([]URL, error) {
	return s.ListAllLifecycle(ctx, p, lifecycle.CurrentOnly)
}

func (s *service) ListAllLifecycle(ctx context.Context, p authz.Principal, visibility lifecycle.Visibility) ([]URL, error) {
	if !s.authz.Allow(p, authz.Resource{Type: authz.TypeURL}, authz.Read) {
		return nil, ErrForbidden
	}
	return s.store.ListLifecycle(ctx, visibility)
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
		taken, err := s.store.AliasTaken(ctx, alias, existing.ID)
		if err != nil {
			return URL{}, err
		}
		if taken {
			return URL{}, ErrConflict
		}
		existing.Alias = alias
	}
	return s.store.Update(ctx, existing)
}

func (s *service) Delete(ctx context.Context, p authz.Principal, id uuid.UUID) error {
	existing, err := s.store.GetIncludingDisabled(ctx, id)
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
	return s.store.Disable(ctx, id, lifecycle.ActorID(p.ID))
}

func (s *service) Restore(ctx context.Context, p authz.Principal, id uuid.UUID) (URL, error) {
	existing, err := s.store.GetIncludingDisabled(ctx, id)
	if err != nil {
		return URL{}, err
	}
	owner := ""
	if existing.CreatedBy != nil {
		owner = existing.CreatedBy.String()
	}
	if !s.authz.Allow(p, authz.Resource{Type: authz.TypeURL, OwnerID: owner}, authz.Delete) {
		return URL{}, ErrForbidden
	}
	if err := s.store.Restore(ctx, id); err != nil {
		return URL{}, err
	}
	return s.store.Get(ctx, id)
}

func (s *service) ensureAlias(ctx context.Context, alias string) (string, error) {
	if strings.TrimSpace(alias) == "" {
		for range 8 {
			cand, err := randomAlias()
			if err != nil {
				return "", err
			}
			taken, err := s.store.AliasTaken(ctx, cand, uuid.Nil)
			if err != nil {
				return "", err
			}
			if !taken {
				return cand, nil
			}
		}
		return "", ErrConflict
	}
	if err := validateAlias(alias); err != nil {
		return "", err
	}
	taken, err := s.store.AliasTaken(ctx, alias, uuid.Nil)
	if err != nil {
		return "", err
	}
	if taken {
		return "", ErrConflict
	}
	return alias, nil
}

func validateAlias(alias string) error {
	if !aliasPattern.MatchString(alias) || reservedAlias(alias) {
		return ErrInvalid
	}
	return nil
}

// reservedAlias names the first path segments skyl.app already routes
// elsewhere; "c" is the certificate page, so skyl.app/c/ig would never reach
// a link called c.
func reservedAlias(alias string) bool {
	switch strings.ToLower(alias) {
	case "v1", "health", "go", "urls", "api", "docs", "c":
		return true
	}
	return false
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
