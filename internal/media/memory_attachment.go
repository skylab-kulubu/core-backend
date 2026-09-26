package media

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// Attach models the database's Media attachment and its triggers: only a
// current Media is attached, and it becomes attached with no expiry.
func (s *MemoryStore) Attach(_ context.Context, a Attachment) (Attachment, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, ok := s.findAttachment(a); ok {
		return existing, false, nil
	}
	m, ok := s.byID[a.MediaID]
	if !ok || m.DeletedAt != nil || m.BlobPurgeStartedAt != nil || m.BlobPurgedAt != nil {
		return Attachment{}, false, ErrNotLinkable
	}
	a.ID = uuid.New()
	a.CreatedAt = time.Now().UTC()
	s.attachments[a.ID] = a
	m.Status = StatusAttached
	m.ExpiresAt = nil
	m.UpdatedAt = a.CreatedAt
	s.byID[m.ID] = m
	return a, true, nil
}

// detachedWindow is how long a detached Media is kept. The database fixes the
// same 30 days in its status trigger (migration 20260926120000); the memory
// store models it here.
const detachedWindow = 30 * 24 * time.Hour

// Detach models the database's status trigger: the Media's last Media
// attachment going detaches it.
func (s *MemoryStore) Detach(_ context.Context, mediaID, attachmentID uuid.UUID, service string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.attachments[attachmentID]
	if !ok || a.MediaID != mediaID || a.Owner.Service != service {
		return nil
	}
	delete(s.attachments, attachmentID)
	for _, other := range s.attachments {
		if other.MediaID == mediaID {
			return nil
		}
	}
	m, ok := s.byID[mediaID]
	if !ok || m.Status != StatusAttached {
		return nil
	}
	now := time.Now().UTC()
	m.Status = StatusDetached
	m.ExpiresAt = nil
	if m.Purpose != PurposeLegacy {
		expires := now.Add(detachedWindow)
		m.ExpiresAt = &expires
	}
	m.UpdatedAt = now
	s.byID[mediaID] = m
	return nil
}

func (s *MemoryStore) FindAttachment(_ context.Context, a Attachment) (Attachment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, ok := s.findAttachment(a); ok {
		return existing, nil
	}
	return Attachment{}, ErrNotFound
}

func (s *MemoryStore) findAttachment(a Attachment) (Attachment, bool) {
	for _, existing := range s.attachments {
		if existing.MediaID == a.MediaID && existing.Owner == a.Owner && existing.Role == a.Role {
			return existing, true
		}
	}
	return Attachment{}, false
}

func (s *MemoryStore) GetAttachment(_ context.Context, mediaID, attachmentID uuid.UUID) (Attachment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.attachments[attachmentID]
	if !ok || a.MediaID != mediaID {
		return Attachment{}, ErrNotFound
	}
	return a, nil
}
