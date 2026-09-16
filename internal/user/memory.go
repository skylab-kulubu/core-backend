package user

import (
	"context"
	"sync"
	"time"

	"github.com/google/uuid"
)

type MemoryStore struct {
	mu   sync.Mutex
	byID map[uuid.UUID]User
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{byID: make(map[uuid.UUID]User)}
}

func (s *MemoryStore) Get(_ context.Context, id uuid.UUID) (User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.byID[id]
	if !ok {
		return User{}, ErrNotFound
	}
	return u, nil
}

func (s *MemoryStore) Upsert(_ context.Context, u User) (User, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC()
	_, existed := s.byID[u.ID]
	if existed {
		u.CreatedAt = s.byID[u.ID].CreatedAt
		u.UpdatedAt = now
	} else {
		u.CreatedAt = now
		u.UpdatedAt = now
	}
	s.byID[u.ID] = u
	return u, !existed, nil
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
