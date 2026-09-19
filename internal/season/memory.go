package season

import (
	"context"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/lifecycle"
)

type MemoryStore struct {
	mu   sync.Mutex
	byID map[uuid.UUID]Season
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{byID: make(map[uuid.UUID]Season)}
}

func (s *MemoryStore) List(_ context.Context, activeOnly bool) ([]Season, error) {
	return s.list(activeOnly, lifecycle.CurrentOnly), nil
}

func (s *MemoryStore) ListLifecycle(_ context.Context, visibility lifecycle.Visibility) ([]Season, error) {
	return s.list(false, visibility), nil
}

func (s *MemoryStore) list(activeOnly bool, visibility lifecycle.Visibility) []Season {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Season, 0, len(s.byID))
	for _, item := range s.byID {
		if !visibility.Matches(item.ArchivedAt != nil) {
			continue
		}
		if activeOnly && !item.Active {
			continue
		}
		out = append(out, item)
	}
	return out
}

func (s *MemoryStore) Get(_ context.Context, id uuid.UUID) (Season, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	item, ok := s.byID[id]
	if !ok || item.ArchivedAt != nil {
		return Season{}, ErrNotFound
	}
	return item, nil
}

func (s *MemoryStore) GetIncludingArchived(_ context.Context, id uuid.UUID) (Season, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	item, ok := s.byID[id]
	if !ok {
		return Season{}, ErrNotFound
	}
	return item, nil
}

func (s *MemoryStore) Create(_ context.Context, in Season) (Season, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if in.ID == uuid.Nil {
		in.ID = uuid.New()
	}
	now := time.Now().UTC()
	in.CreatedAt = now
	in.UpdatedAt = now
	s.byID[in.ID] = in
	return in, nil
}

func (s *MemoryStore) Update(_ context.Context, in Season) (Season, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	existing, ok := s.byID[in.ID]
	if !ok || existing.ArchivedAt != nil {
		return Season{}, ErrNotFound
	}
	in.CreatedAt = existing.CreatedAt
	in.UpdatedAt = time.Now().UTC()
	s.byID[in.ID] = in
	return in, nil
}

func (s *MemoryStore) Archive(_ context.Context, id uuid.UUID, actorID *uuid.UUID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	item, ok := s.byID[id]
	if !ok {
		return ErrNotFound
	}
	if item.ArchivedAt == nil {
		now := time.Now().UTC()
		item.ArchivedAt = &now
		item.ArchivedBy = actorID
		item.UpdatedAt = now
		s.byID[id] = item
	}
	return nil
}

func (s *MemoryStore) Restore(_ context.Context, id uuid.UUID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	item, ok := s.byID[id]
	if !ok {
		return ErrNotFound
	}
	if item.ArchivedAt != nil {
		item.ArchivedAt = nil
		item.ArchivedBy = nil
		item.UpdatedAt = time.Now().UTC()
		s.byID[id] = item
	}
	return nil
}
