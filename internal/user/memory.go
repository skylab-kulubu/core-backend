package user

import (
	"context"
	"strings"
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
	existing, existed := s.byID[u.ID]
	if existed {
		if u.SchoolEmail == "" {
			u.SchoolEmail = existing.SchoolEmail
		}
		u.CreatedAt = existing.CreatedAt
		u.UpdatedAt = now
	} else {
		u.CreatedAt = now
		u.UpdatedAt = now
	}
	s.byID[u.ID] = u
	return u, !existed, nil
}

func (s *MemoryStore) Search(_ context.Context, q string) ([]User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	needle := strings.ToLower(strings.TrimSpace(q))
	out := make([]User, 0)
	for _, u := range s.byID {
		if userMatches(u, needle) {
			out = append(out, u)
		}
	}
	return out, nil
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

func userMatches(u User, needle string) bool {
	if needle == "" {
		return true
	}
	hay := []string{
		u.Email,
		u.SchoolEmail,
		u.FirstName,
		u.LastName,
		strings.TrimSpace(u.FirstName + " " + u.LastName),
	}
	for _, h := range hay {
		if strings.Contains(strings.ToLower(h), needle) {
			return true
		}
	}
	return false
}
