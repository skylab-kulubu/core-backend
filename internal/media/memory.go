package media

import (
	"bytes"
	"context"
	"io"
	"maps"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/lifecycle"
)

type MemoryStore struct {
	mu          sync.Mutex
	byID        map[uuid.UUID]Media
	referenced  map[uuid.UUID]bool
	attachments map[uuid.UUID]Attachment
	readLinks   []ReadLinkRecord
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		byID:        make(map[uuid.UUID]Media),
		referenced:  make(map[uuid.UUID]bool),
		attachments: make(map[uuid.UUID]Attachment),
	}
}

func (s *MemoryStore) Create(_ context.Context, m Media) (Media, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m = newRecord(m)
	now := time.Now().UTC()
	m.CreatedAt = now
	m.UpdatedAt = now
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

func (s *MemoryStore) ListPendingServingPolicy(_ context.Context, after uuid.UUID, limit int) ([]Media, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Media, 0)
	for _, m := range s.byID {
		if m.ServingPolicyApplied || m.BlobPurgeStartedAt != nil || m.BlobPurgedAt != nil || bytes.Compare(m.ID[:], after[:]) <= 0 {
			continue
		}
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return bytes.Compare(out[i].ID[:], out[j].ID[:]) < 0 })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (s *MemoryStore) SetServingPolicyApplied(_ context.Context, id uuid.UUID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.byID[id]
	if !ok {
		return ErrNotFound
	}
	m.ServingPolicyApplied = true
	s.byID[id] = m
	return nil
}

func (s *MemoryStore) ListPendingImageSizes(_ context.Context, purposes []string, after uuid.UUID, limit int) ([]Media, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Media, 0)
	for _, m := range s.byID {
		if m.Kind != KindImage || m.SizeObjects != nil || !slices.Contains(purposes, m.Purpose) || m.DeletedAt != nil || m.BlobPurgeStartedAt != nil || m.BlobPurgedAt != nil || bytes.Compare(m.ID[:], after[:]) <= 0 {
			continue
		}
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return bytes.Compare(out[i].ID[:], out[j].ID[:]) < 0 })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (s *MemoryStore) SetImageSizes(_ context.Context, id uuid.UUID, size ImageSize, objects map[string]SizeObject) error {
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
	m.Width, m.Height = size.Width, size.Height
	m.SizeObjects = maps.Clone(objects)
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
		m.ExpiresAt = nil
		m.UpdatedAt = time.Now().UTC()
		s.byID[id] = m
	}
	return nil
}

func (s *MemoryStore) ExpireUnattachedAt(_ context.Context, id uuid.UUID, at *time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.byID[id]
	if !ok || m.DeletedAt != nil {
		return ErrNotFound
	}
	if m.Status != StatusAttached && m.BlobPurgeStartedAt == nil && m.BlobPurgedAt == nil {
		m.ExpiresAt = at
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
	if err := purgeObjects(m.Key, purge); err != nil {
		return false, err
	}
	m.BlobPurgedAt = &purgedAt
	m.BlobPurgeCheckedAt = &purgedAt
	m.UpdatedAt = purgedAt
	s.byID[id] = m
	return true, nil
}

func (s *MemoryStore) ListExpired(_ context.Context, now time.Time, after uuid.UUID, limit int) ([]Media, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Media, 0)
	for _, m := range s.byID {
		if m.BlobPurgedAt != nil || !m.expired(now) || (m.DeletedAt != nil && m.BlobPurgeStartedAt == nil) || bytes.Compare(m.ID[:], after[:]) <= 0 {
			continue
		}
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return bytes.Compare(out[i].ID[:], out[j].ID[:]) < 0 })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (s *MemoryStore) PurgeExpiredBlobIfUnattached(_ context.Context, id uuid.UUID, now time.Time, purge func(string) error) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.byID[id]
	if !ok {
		return false, ErrNotFound
	}
	if m.BlobPurgedAt != nil || (m.BlobPurgeStartedAt == nil && (m.DeletedAt != nil || !m.expired(now))) {
		return false, nil
	}
	if s.referenced[id] || m.Status == StatusAttached {
		m.BlobPurgeStartedAt = nil
		m.BlobPurgeCheckedAt = &now
		s.byID[id] = m
		return false, nil
	}
	if m.BlobPurgeStartedAt == nil {
		m.BlobPurgeStartedAt = &now
		m.BlobPurgeCheckedAt = &now
		s.byID[id] = m
	}
	if err := purgeObjects(m.Key, purge); err != nil {
		return false, err
	}
	m.BlobPurgedAt = &now
	m.BlobPurgeCheckedAt = &now
	if m.DeletedAt == nil {
		m.DeletedAt = &now
	}
	m.UpdatedAt = now
	s.byID[id] = m
	return true, nil
}

// SetReferenced models a durable domain reference in in-memory service tests.
func (s *MemoryStore) SetReferenced(id uuid.UUID, referenced bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.referenced[id] = referenced
}

func (s *MemoryStore) RecordReadLink(_ context.Context, link ReadLinkRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.byID[link.MediaID]; !ok {
		return ErrNotFound
	}
	link.Opens = nil
	s.readLinks = append(s.readLinks, link)
	return nil
}

func (s *MemoryStore) RecordReadLinkOpen(_ context.Context, open ReadLinkOpen) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.readLinks {
		if s.readLinks[i].ID == open.LinkID {
			s.readLinks[i].Opens = append(s.readLinks[i].Opens, open)
			return nil
		}
	}
	return ErrNotFound
}

// ReadLinkLog is the Media's access log, oldest first, each link with its
// opens: what a test checks core recorded.
func (s *MemoryStore) ReadLinkLog(mediaID uuid.UUID) []ReadLinkRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []ReadLinkRecord{}
	for _, link := range s.readLinks {
		if link.MediaID == mediaID {
			link.Opens = append([]ReadLinkOpen(nil), link.Opens...)
			out = append(out, link)
		}
	}
	return out
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

func (s *MemoryBlob) SetMetadata(_ context.Context, key string, meta BlobMetadata) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.objects[key]; !ok {
		return ErrNotFound
	}
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

// Open streams a stored object.
func (s *MemoryBlob) Open(ctx context.Context, key string) (io.ReadCloser, error) {
	data, err := s.Read(ctx, key)
	if err != nil {
		return nil, err
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

// Len is how many objects the bucket holds.
func (s *MemoryBlob) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.objects)
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

// Keys are the keys of every stored object, in order.
func (s *MemoryBlob) Keys() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Sorted(maps.Keys(s.objects))
}
