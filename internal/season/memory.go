package season

import (
	"context"
	"sync"
	"time"

	"github.com/google/uuid"
)

type MemoryStore struct {
	mu   sync.Mutex
	byID map[uuid.UUID]Season
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{byID: make(map[uuid.UUID]Season)}
}

func (s *MemoryStore) List(_ context.Context, activeOnly bool) ([]Season, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Season, 0, len(s.byID))
	for _, item := range s.byID {
		if activeOnly && !item.Active {
			continue
		}
		out = append(out, item)
	}
	return out, nil
}

func (s *MemoryStore) Get(_ context.Context, id uuid.UUID) (Season, error) {
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
	if !ok {
		return Season{}, ErrNotFound
	}
	in.CreatedAt = existing.CreatedAt
	in.UpdatedAt = time.Now().UTC()
	s.byID[in.ID] = in
	return in, nil
}

func (s *MemoryStore) Delete(_ context.Context, id uuid.UUID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.byID[id]; !ok {
		return ErrNotFound
	}
	delete(s.byID, id)
	return nil
}
