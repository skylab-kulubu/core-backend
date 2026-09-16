package event

import (
	"context"
	"sync"
	"time"

	"github.com/google/uuid"
)

type MemoryStore struct {
	mu   sync.Mutex
	byID map[uuid.UUID]Event
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{byID: make(map[uuid.UUID]Event)}
}

func (s *MemoryStore) List(_ context.Context, ownerTeam string) ([]Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Event, 0, len(s.byID))
	for _, e := range s.byID {
		if ownerTeam != "" && e.OwnerTeam != ownerTeam {
			continue
		}
		out = append(out, e)
	}
	return out, nil
}

func (s *MemoryStore) Get(_ context.Context, id uuid.UUID) (Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.byID[id]
	if !ok {
		return Event{}, ErrNotFound
	}
	return e, nil
}

func (s *MemoryStore) Create(_ context.Context, e Event) (Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e.ID == uuid.Nil {
		e.ID = uuid.New()
	}
	now := time.Now().UTC()
	e.CreatedAt = now
	e.UpdatedAt = now
	s.byID[e.ID] = e
	return e, nil
}

func (s *MemoryStore) Update(_ context.Context, e Event) (Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	existing, ok := s.byID[e.ID]
	if !ok {
		return Event{}, ErrNotFound
	}
	e.CreatedAt = existing.CreatedAt
	e.UpdatedAt = time.Now().UTC()
	s.byID[e.ID] = e
	return e, nil
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
