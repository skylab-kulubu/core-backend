package competitor

import (
	"context"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/event"
)

type MemoryStore struct {
	mu     sync.Mutex
	byID   map[uuid.UUID]Competitor
	events event.Store
}

func NewMemoryStore(events event.Store) *MemoryStore {
	return &MemoryStore{
		byID:   make(map[uuid.UUID]Competitor),
		events: events,
	}
}

func (s *MemoryStore) List(_ context.Context) ([]Competitor, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Competitor, 0, len(s.byID))
	for _, c := range s.byID {
		out = append(out, c)
	}
	return out, nil
}

func (s *MemoryStore) Get(_ context.Context, id uuid.UUID) (Competitor, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.byID[id]
	if !ok {
		return Competitor{}, ErrNotFound
	}
	return c, nil
}

func (s *MemoryStore) Create(_ context.Context, c Competitor) (Competitor, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c.ID == uuid.Nil {
		c.ID = uuid.New()
	}
	now := time.Now().UTC()
	c.CreatedAt = now
	c.UpdatedAt = now
	s.byID[c.ID] = c
	return c, nil
}

func (s *MemoryStore) Update(_ context.Context, c Competitor) (Competitor, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	existing, ok := s.byID[c.ID]
	if !ok {
		return Competitor{}, ErrNotFound
	}
	c.CreatedAt = existing.CreatedAt
	c.UpdatedAt = time.Now().UTC()
	s.byID[c.ID] = c
	return c, nil
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

func (s *MemoryStore) ListByEvent(_ context.Context, eventID uuid.UUID) ([]Competitor, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Competitor, 0)
	for _, c := range s.byID {
		if c.EventID == eventID {
			out = append(out, c)
		}
	}
	return out, nil
}

func (s *MemoryStore) ListByUser(_ context.Context, userID uuid.UUID) ([]Competitor, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Competitor, 0)
	for _, c := range s.byID {
		if c.UserID == userID {
			out = append(out, c)
		}
	}
	return out, nil
}

func (s *MemoryStore) ListByOwnerTeam(ctx context.Context, ownerTeam string) ([]Competitor, error) {
	s.mu.Lock()
	snapshot := make([]Competitor, 0, len(s.byID))
	for _, c := range s.byID {
		snapshot = append(snapshot, c)
	}
	s.mu.Unlock()

	out := make([]Competitor, 0)
	for _, c := range snapshot {
		ev, err := s.events.Get(ctx, c.EventID)
		if err != nil {
			continue
		}
		if ev.OwnerTeam == ownerTeam {
			out = append(out, c)
		}
	}
	return out, nil
}

func (s *MemoryStore) ExistsUserEvent(_ context.Context, userID, eventID uuid.UUID) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.byID {
		if c.UserID == userID && c.EventID == eventID {
			return true, nil
		}
	}
	return false, nil
}

func (s *MemoryStore) Winner(_ context.Context, eventID uuid.UUID) (Competitor, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.byID {
		if c.EventID == eventID && c.IsWinner {
			return c, nil
		}
	}
	return Competitor{}, ErrNotFound
}
