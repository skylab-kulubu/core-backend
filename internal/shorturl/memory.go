package shorturl

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/lifecycle"
)

type MemoryStore struct {
	mu   sync.Mutex
	byID map[uuid.UUID]URL
	hits []Hit
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{byID: map[uuid.UUID]URL{}}
}

func (s *MemoryStore) Create(_ context.Context, u URL) (URL, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if u.ID == uuid.Nil {
		u.ID = uuid.New()
	}
	now := time.Now().UTC()
	if u.CreatedAt.IsZero() {
		u.CreatedAt = now
	}
	u.UpdatedAt = now
	if _, err := s.findAliasLocked(u.Alias, uuid.Nil, true); err == nil {
		return URL{}, ErrConflict
	}
	s.byID[u.ID] = u
	return u, nil
}

func (s *MemoryStore) Get(_ context.Context, id uuid.UUID) (URL, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.byID[id]
	if !ok || u.DisabledAt != nil {
		return URL{}, ErrNotFound
	}
	return u, nil
}

func (s *MemoryStore) GetIncludingDisabled(_ context.Context, id uuid.UUID) (URL, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.byID[id]
	if !ok {
		return URL{}, ErrNotFound
	}
	return u, nil
}

func (s *MemoryStore) GetByAlias(_ context.Context, alias string) (URL, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.findAliasLocked(alias, uuid.Nil, false)
}

func (s *MemoryStore) ListByCreator(_ context.Context, userID uuid.UUID) ([]URL, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]URL, 0)
	for _, u := range s.byID {
		if u.DisabledAt == nil && u.CreatedBy != nil && *u.CreatedBy == userID {
			out = append(out, u)
		}
	}
	return out, nil
}

func (s *MemoryStore) ListAll(_ context.Context) ([]URL, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]URL, 0, len(s.byID))
	for _, u := range s.byID {
		if u.DisabledAt == nil {
			out = append(out, u)
		}
	}
	return out, nil
}

func (s *MemoryStore) ListLifecycle(_ context.Context, visibility lifecycle.Visibility) ([]URL, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]URL, 0, len(s.byID))
	for _, u := range s.byID {
		if !visibility.Matches(u.DisabledAt != nil) {
			continue
		}
		out = append(out, u)
	}
	return out, nil
}

func (s *MemoryStore) Update(_ context.Context, u URL) (URL, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	existing, ok := s.byID[u.ID]
	if !ok || existing.DisabledAt != nil {
		return URL{}, ErrNotFound
	}
	if _, err := s.findAliasLocked(u.Alias, u.ID, true); err == nil {
		return URL{}, ErrConflict
	}
	u.UpdatedAt = time.Now().UTC()
	s.byID[u.ID] = u
	return u, nil
}

func (s *MemoryStore) Delete(_ context.Context, id uuid.UUID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.byID[id]; !ok {
		return ErrNotFound
	}
	delete(s.byID, id)
	kept := s.hits[:0]
	for _, h := range s.hits {
		if h.URLID != id {
			kept = append(kept, h)
		}
	}
	s.hits = kept
	return nil
}

func (s *MemoryStore) RecordHit(_ context.Context, id uuid.UUID, hit Hit) (URL, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.byID[id]
	if !ok || u.DisabledAt != nil {
		return URL{}, ErrNotFound
	}
	if hit.ID == uuid.Nil {
		hit.ID = uuid.New()
	}
	if hit.CreatedAt.IsZero() {
		hit.CreatedAt = time.Now().UTC()
	}
	hit.URLID = u.ID
	hit.Alias = u.Alias
	s.hits = append(s.hits, hit)
	u.ClickCount = s.pruneLocked(id)
	u.UpdatedAt = time.Now().UTC()
	s.byID[id] = u
	return u, nil
}

func (s *MemoryStore) ListHits(_ context.Context, id uuid.UUID, since time.Time) ([]Hit, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.byID[id]
	if !ok || u.DisabledAt != nil {
		return nil, ErrNotFound
	}
	out := make([]Hit, 0)
	for i := len(s.hits) - 1; i >= 0; i-- {
		h := s.hits[i]
		if h.URLID == id && !h.CreatedAt.Before(since) {
			out = append(out, h)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].CreatedAt.After(out[j].CreatedAt)
	})
	return out, nil
}

func (s *MemoryStore) pruneLocked(id uuid.UUID) int {
	cutoff := time.Now().UTC().Add(-HitRetention)
	next := make([]Hit, 0, len(s.hits))
	n := 0
	for _, h := range s.hits {
		if h.URLID == id && h.CreatedAt.Before(cutoff) {
			continue
		}
		next = append(next, h)
		if h.URLID == id {
			n++
		}
	}
	s.hits = next
	return n
}

func (s *MemoryStore) findAliasLocked(alias string, except uuid.UUID, includeDisabled bool) (URL, error) {
	for _, u := range s.byID {
		if u.Alias == alias && u.ID != except && (includeDisabled || u.DisabledAt == nil) {
			return u, nil
		}
	}
	return URL{}, ErrNotFound
}
