package media

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
)

// Attach models the database's Media attachment and its triggers: only a
// current Media is attached, and it becomes attached with no expiry.
func (s *MemoryStore) Attach(_ context.Context, a Attachment) (Attachment, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.attach(a)
}

// attach writes the Media attachment; s.mu is held.
func (s *MemoryStore) attach(a Attachment) (Attachment, bool, error) {
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

// AttachHeld models the Postgres store's transaction: a held Media whose
// purpose does not fit the role goes back to legacy only together with the
// new Media attachment. When the same link is already there, or the Media
// cannot be linked, nothing changes.
func (s *MemoryStore) AttachHeld(_ context.Context, a Attachment) (Attachment, bool, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.byID[a.MediaID]
	if !ok || !m.DetachExpiryHeld || m.DeletedAt != nil || m.BlobPurgeStartedAt != nil || m.BlobPurgedAt != nil {
		return Attachment{}, false, "", ErrNotLinkable
	}
	if existing, ok := s.findAttachment(a); ok {
		return existing, false, "", nil
	}
	demotedFrom := ""
	if !fits(a.Owner.Service, a.Role, m.Purpose) {
		demotedFrom = m.Purpose
		m.Purpose = PurposeLegacy
		m.UpdatedAt = time.Now().UTC()
		s.byID[m.ID] = m
	}
	created, isNew, err := s.attach(a)
	if err != nil {
		return Attachment{}, false, "", err
	}
	return created, isNew, demotedFrom, nil
}

// Detach models the database's status trigger: the Media's last Media
// attachment going detaches it.
func (s *MemoryStore) Detach(_ context.Context, mediaID, attachmentID uuid.UUID, service authz.Product) error {
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
	if m.Purpose != PurposeLegacy && !m.DetachExpiryHeld {
		// The database's status trigger fixes the detached window at the
		// default recovery window, 30 days (migration 20260926120000); a
		// held Media gets none (20260926161000).
		expires := now.Add(DefaultBlobRecoveryWindow)
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

func (s *MemoryStore) HeldBy(_ context.Context, mediaID uuid.UUID, product authz.Product) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range s.attachments {
		if a.MediaID == mediaID && a.Owner.Service == product {
			return true, nil
		}
	}
	return false, nil
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
