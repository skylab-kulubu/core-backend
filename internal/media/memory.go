package media

import (
	"context"
	"sync"
	"time"

	"github.com/google/uuid"
)

type MemoryStore struct {
	mu   sync.Mutex
	byID map[uuid.UUID]Media
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{byID: make(map[uuid.UUID]Media)}
}

func (s *MemoryStore) Create(_ context.Context, m Media) (Media, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if m.ID == uuid.Nil {
		m.ID = uuid.New()
	}
	now := time.Now().UTC()
	m.CreatedAt = now
	m.UpdatedAt = now
	s.byID[m.ID] = m
	return m, nil
}

func (s *MemoryStore) Get(_ context.Context, id uuid.UUID) (Media, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.byID[id]
	if !ok {
		return Media{}, ErrNotFound
	}
	return m, nil
}

func (s *MemoryStore) List(_ context.Context) ([]Media, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Media, 0, len(s.byID))
	for _, m := range s.byID {
		out = append(out, m)
	}
	return out, nil
}

func (s *MemoryStore) Delete(_ context.Context, id uuid.UUID) (Media, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.byID[id]
	if !ok {
		return Media{}, ErrNotFound
	}
	delete(s.byID, id)
	return m, nil
}

type MemoryBlob struct {
	mu      sync.Mutex
	objects map[string][]byte
	types   map[string]string
}

func NewMemoryBlob() *MemoryBlob {
	return &MemoryBlob{objects: make(map[string][]byte), types: make(map[string]string)}
}

func (s *MemoryBlob) Put(_ context.Context, key string, data []byte, contentType string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := append([]byte{}, data...)
	s.objects[key] = cp
	s.types[key] = contentType
	return nil
}

func (s *MemoryBlob) Delete(_ context.Context, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.objects, key)
	delete(s.types, key)
	return nil
}

func (s *MemoryBlob) Get(key string) ([]byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, ok := s.objects[key]
	return data, ok
}
