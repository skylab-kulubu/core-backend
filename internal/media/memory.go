package media

import (
	"context"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/lifecycle"
)

type MemoryStore struct {
	mu         sync.Mutex
	byID       map[uuid.UUID]Media
	referenced map[uuid.UUID]bool
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{byID: make(map[uuid.UUID]Media), referenced: make(map[uuid.UUID]bool)}
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
	if m.CoverColors == nil {
		m.CoverColors = []string{}
	}
	s.byID[m.ID] = m
	return m, nil
}

func (s *MemoryStore) ListPendingCoverColors(_ context.Context, limit int) ([]Media, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Media, 0)
	for _, m := range s.byID {
		if m.DeletedAt != nil || m.Kind != KindImage || m.CoverColorsComputed {
			continue
		}
		out = append(out, m)
		if limit > 0 && len(out) == limit {
			break
		}
	}
	return out, nil
}

func (s *MemoryStore) SetCoverColors(_ context.Context, id uuid.UUID, colors []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.byID[id]
	if !ok || m.DeletedAt != nil {
		return ErrNotFound
	}
	m.CoverColors = append([]string{}, colors...)
	m.CoverColorsComputed = true
	m.UpdatedAt = time.Now().UTC()
	s.byID[id] = m
	return nil
}

func (s *MemoryStore) Get(_ context.Context, id uuid.UUID) (Media, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.byID[id]
	if !ok || m.DeletedAt != nil {
		return Media{}, ErrNotFound
	}
	return m, nil
}

func (s *MemoryStore) List(_ context.Context) ([]Media, error) {
	return s.list(lifecycle.CurrentOnly), nil
}

func (s *MemoryStore) ListLifecycle(_ context.Context, visibility lifecycle.Visibility) ([]Media, error) {
	return s.list(visibility), nil
}

func (s *MemoryStore) list(visibility lifecycle.Visibility) []Media {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Media, 0, len(s.byID))
	for _, m := range s.byID {
		if !visibility.Matches(m.DeletedAt != nil) {
			continue
		}
		out = append(out, m)
	}
	return out
}

func (s *MemoryStore) GetIncludingDeleted(_ context.Context, id uuid.UUID) (Media, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.byID[id]
	if !ok {
		return Media{}, ErrNotFound
	}
	return m, nil
}

func (s *MemoryStore) Archive(_ context.Context, id uuid.UUID, actorID *uuid.UUID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.byID[id]
	if !ok {
		return ErrNotFound
	}
	if m.DeletedAt == nil {
		now := time.Now().UTC()
		m.DeletedAt = &now
		m.DeletedBy = actorID
		m.UpdatedAt = now
		s.byID[id] = m
	}
	return nil
}

func (s *MemoryStore) Restore(_ context.Context, id uuid.UUID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.byID[id]
	if !ok {
		return ErrNotFound
	}
	if m.BlobPurgedAt != nil {
		return ErrPurged
	}
	if m.BlobPurgeStartedAt != nil {
		return ErrPurgeInProgress
	}
	if m.DeletedAt != nil {
		m.DeletedAt = nil
		m.DeletedBy = nil
		m.UpdatedAt = time.Now().UTC()
		s.byID[id] = m
	}
	return nil
}

func (s *MemoryStore) ListPurgeCandidates(_ context.Context, deletedBefore time.Time, limit int) ([]Media, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Media, 0)
	for _, m := range s.byID {
		if m.DeletedAt == nil || m.DeletedAt.After(deletedBefore) || m.BlobPurgedAt != nil {
			continue
		}
		out = append(out, m)
		if limit > 0 && len(out) == limit {
			break
		}
	}
	return out, nil
}

func (s *MemoryStore) PurgeBlobIfUnreferenced(_ context.Context, id uuid.UUID, purgedAt time.Time, purge func(string) error) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.byID[id]
	if !ok {
		return false, ErrNotFound
	}
	if m.DeletedAt == nil || m.BlobPurgedAt != nil {
		return false, nil
	}
	if s.referenced[id] {
		m.BlobPurgeStartedAt = nil
		m.BlobPurgeCheckedAt = &purgedAt
		s.byID[id] = m
		return false, nil
	}
	if m.BlobPurgeStartedAt == nil {
		m.BlobPurgeStartedAt = &purgedAt
		m.BlobPurgeCheckedAt = &purgedAt
		s.byID[id] = m
	}
	if err := purge(m.Key); err != nil {
		return false, err
	}
	m.BlobPurgedAt = &purgedAt
	m.BlobPurgeCheckedAt = &purgedAt
	m.UpdatedAt = purgedAt
	s.byID[id] = m
	return true, nil
}

// SetReferenced models a durable domain reference in in-memory service tests.
func (s *MemoryStore) SetReferenced(id uuid.UUID, referenced bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.referenced[id] = referenced
}

type MemoryBlob struct {
	mu       sync.Mutex
	objects  map[string][]byte
	metadata map[string]BlobMetadata
}

func NewMemoryBlob() *MemoryBlob {
	return &MemoryBlob{objects: make(map[string][]byte), metadata: make(map[string]BlobMetadata)}
}

func (s *MemoryBlob) Put(_ context.Context, key string, data []byte, meta BlobMetadata) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := append([]byte{}, data...)
	s.objects[key] = cp
	s.metadata[key] = meta
	return nil
}

func (s *MemoryBlob) Delete(_ context.Context, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.objects, key)
	delete(s.metadata, key)
	return nil
}

func (s *MemoryBlob) Read(_ context.Context, key string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, ok := s.objects[key]
	if !ok {
		return nil, ErrNotFound
	}
	return append([]byte{}, data...), nil
}

func (s *MemoryBlob) Get(key string) ([]byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, ok := s.objects[key]
	return data, ok
}

// Metadata is the serving metadata the object was stored with.
func (s *MemoryBlob) Metadata(key string) (BlobMetadata, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	meta, ok := s.metadata[key]
	return meta, ok
}
