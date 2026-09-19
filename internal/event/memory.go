package event

import (
	"context"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/lifecycle"
)

type MemoryStore struct {
	mu       sync.Mutex
	byID     map[uuid.UUID]Event
	days     map[uuid.UUID]Day
	sessions map[uuid.UUID]Session
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		byID:     make(map[uuid.UUID]Event),
		days:     make(map[uuid.UUID]Day),
		sessions: make(map[uuid.UUID]Session),
	}
}

func (s *MemoryStore) List(_ context.Context, ownerTeam string, activeOnly bool) ([]Event, error) {
	return s.list(ownerTeam, activeOnly, lifecycle.CurrentOnly), nil
}

func (s *MemoryStore) ListLifecycle(_ context.Context, ownerTeam string, visibility lifecycle.Visibility) ([]Event, error) {
	return s.list(ownerTeam, false, visibility), nil
}

func (s *MemoryStore) list(ownerTeam string, activeOnly bool, visibility lifecycle.Visibility) []Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Event, 0, len(s.byID))
	for _, e := range s.byID {
		if !visibility.Matches(e.ArchivedAt != nil) {
			continue
		}
		if ownerTeam != "" && e.OwnerTeam != ownerTeam {
			continue
		}
		if activeOnly && !e.Active {
			continue
		}
		out = append(out, emptyGallery(e))
	}
	return out
}

func (s *MemoryStore) Get(_ context.Context, id uuid.UUID) (Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.byID[id]
	if !ok || e.ArchivedAt != nil {
		return Event{}, ErrNotFound
	}
	return emptyGallery(e), nil
}

func (s *MemoryStore) GetIncludingArchived(_ context.Context, id uuid.UUID) (Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.byID[id]
	if !ok {
		return Event{}, ErrNotFound
	}
	return emptyGallery(e), nil
}

func (s *MemoryStore) Create(_ context.Context, e Event) (Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e.ID == uuid.Nil {
		e.ID = uuid.New()
	}
	if e.AttendanceRule == "" {
		e.AttendanceRule = "none"
	}
	now := time.Now().UTC()
	e.CreatedAt = now
	e.UpdatedAt = now
	e = emptyGallery(e)
	s.byID[e.ID] = e
	return e, nil
}

func (s *MemoryStore) Update(_ context.Context, e Event) (Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	existing, ok := s.byID[e.ID]
	if !ok || existing.ArchivedAt != nil {
		return Event{}, ErrNotFound
	}
	e.CreatedAt = existing.CreatedAt
	e.UpdatedAt = time.Now().UTC()
	e.Images = existing.Images
	e.ImageURLs = existing.ImageURLs
	if e.MailListID == nil {
		e.MailListID = existing.MailListID
	}
	e = emptyGallery(e)
	s.byID[e.ID] = e
	return e, nil
}

func (s *MemoryStore) Archive(_ context.Context, id uuid.UUID, actorID *uuid.UUID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.byID[id]
	if !ok {
		return ErrNotFound
	}
	if e.ArchivedAt == nil {
		now := time.Now().UTC()
		e.ArchivedAt = &now
		e.ArchivedBy = actorID
		e.UpdatedAt = now
		s.byID[id] = e
	}
	return nil
}

func (s *MemoryStore) Restore(_ context.Context, id uuid.UUID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.byID[id]
	if !ok {
		return ErrNotFound
	}
	if e.ArchivedAt != nil {
		e.ArchivedAt = nil
		e.ArchivedBy = nil
		e.UpdatedAt = time.Now().UTC()
		s.byID[id] = e
	}
	return nil
}

func (s *MemoryStore) AddImages(_ context.Context, eventID uuid.UUID, ids []uuid.UUID) (Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.byID[eventID]
	if !ok {
		return Event{}, ErrNotFound
	}
	seen := map[uuid.UUID]struct{}{}
	for _, im := range e.Images {
		seen[im.ID] = struct{}{}
	}
	for _, id := range ids {
		if id == uuid.Nil {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		e.Images = append(e.Images, GalleryImage{ID: id})
	}
	e.ImageURLs = urlsOf(e.Images)
	e.UpdatedAt = time.Now().UTC()
	s.byID[eventID] = e
	return emptyGallery(e), nil
}

func (s *MemoryStore) RemoveImages(_ context.Context, eventID uuid.UUID, ids []uuid.UUID) (Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.byID[eventID]
	if !ok {
		return Event{}, ErrNotFound
	}
	have := map[uuid.UUID]GalleryImage{}
	for _, im := range e.Images {
		have[im.ID] = im
	}
	for _, id := range ids {
		if _, ok := have[id]; !ok {
			return Event{}, ErrNotFound
		}
	}
	drop := map[uuid.UUID]struct{}{}
	for _, id := range ids {
		drop[id] = struct{}{}
	}
	kept := make([]GalleryImage, 0, len(e.Images))
	for _, im := range e.Images {
		if _, skip := drop[im.ID]; skip {
			continue
		}
		kept = append(kept, im)
	}
	e.Images = kept
	e.ImageURLs = urlsOf(e.Images)
	e.UpdatedAt = time.Now().UTC()
	s.byID[eventID] = e
	return emptyGallery(e), nil
}

func urlsOf(images []GalleryImage) []string {
	out := make([]string, 0, len(images))
	for _, im := range images {
		if im.URL != "" {
			out = append(out, im.URL)
		}
	}
	return out
}

func (s *MemoryStore) GetDay(_ context.Context, id uuid.UUID) (Day, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.days[id]
	if !ok || d.ArchivedAt != nil {
		return Day{}, ErrNotFound
	}
	e, ok := s.byID[d.EventID]
	if !ok || e.ArchivedAt != nil {
		return Day{}, ErrNotFound
	}
	return d, nil
}

func (s *MemoryStore) GetDayIncludingArchived(_ context.Context, id uuid.UUID) (Day, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.days[id]
	if !ok {
		return Day{}, ErrNotFound
	}
	return d, nil
}

func (s *MemoryStore) CreateDay(_ context.Context, d Day) (Day, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if d.ID == uuid.Nil {
		d.ID = uuid.New()
	}
	s.days[d.ID] = d
	return d, nil
}

func (s *MemoryStore) ListDays(_ context.Context, eventID uuid.UUID) ([]Day, error) {
	return s.listDays(eventID, lifecycle.CurrentOnly, true), nil
}

func (s *MemoryStore) ListDaysLifecycle(_ context.Context, eventID uuid.UUID, visibility lifecycle.Visibility) ([]Day, error) {
	return s.listDays(eventID, visibility, false), nil
}

func (s *MemoryStore) listDays(eventID uuid.UUID, visibility lifecycle.Visibility, requireCurrentParent bool) []Day {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Day, 0)
	if requireCurrentParent {
		e, ok := s.byID[eventID]
		if !ok || e.ArchivedAt != nil {
			return out
		}
	}
	for _, d := range s.days {
		if !visibility.Matches(d.ArchivedAt != nil) {
			continue
		}
		if d.EventID == eventID {
			out = append(out, d)
		}
	}
	return out
}

func (s *MemoryStore) UpdateDay(_ context.Context, d Day) (Day, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	existing, ok := s.days[d.ID]
	if !ok || existing.ArchivedAt != nil {
		return Day{}, ErrNotFound
	}
	s.days[d.ID] = d
	return d, nil
}

func (s *MemoryStore) ArchiveDay(_ context.Context, id uuid.UUID, actorID *uuid.UUID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.days[id]
	if !ok {
		return ErrNotFound
	}
	if d.ArchivedAt == nil {
		now := time.Now().UTC()
		d.ArchivedAt = &now
		d.ArchivedBy = actorID
		s.days[id] = d
	}
	return nil
}

func (s *MemoryStore) RestoreDay(_ context.Context, id uuid.UUID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.days[id]
	if !ok {
		return ErrNotFound
	}
	if d.ArchivedAt != nil {
		d.ArchivedAt = nil
		d.ArchivedBy = nil
		s.days[id] = d
	}
	return nil
}

func (s *MemoryStore) ListBySeason(_ context.Context, seasonID uuid.UUID) ([]Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Event, 0)
	for _, e := range s.byID {
		if e.ArchivedAt == nil && e.SeasonID != nil && *e.SeasonID == seasonID {
			out = append(out, emptyGallery(e))
		}
	}
	return out, nil
}

func (s *MemoryStore) SetSeason(_ context.Context, eventID uuid.UUID, seasonID *uuid.UUID) (Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.byID[eventID]
	if !ok {
		return Event{}, ErrNotFound
	}
	e.SeasonID = seasonID
	e.UpdatedAt = time.Now().UTC()
	s.byID[eventID] = e
	return emptyGallery(e), nil
}

func (s *MemoryStore) SetMailListID(_ context.Context, eventID, listID uuid.UUID) (Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.byID[eventID]
	if !ok {
		return Event{}, ErrNotFound
	}
	id := listID
	e.MailListID = &id
	e.UpdatedAt = time.Now().UTC()
	s.byID[eventID] = e
	return emptyGallery(e), nil
}

func (s *MemoryStore) GetSession(_ context.Context, id uuid.UUID) (Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[id]
	if !ok || sess.ArchivedAt != nil {
		return Session{}, ErrNotFound
	}
	d, ok := s.days[sess.EventDayID]
	if !ok || d.ArchivedAt != nil {
		return Session{}, ErrNotFound
	}
	e, ok := s.byID[d.EventID]
	if !ok || e.ArchivedAt != nil {
		return Session{}, ErrNotFound
	}
	return sess, nil
}

func (s *MemoryStore) GetSessionIncludingArchived(_ context.Context, id uuid.UUID) (Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[id]
	if !ok {
		return Session{}, ErrNotFound
	}
	return sess, nil
}

func (s *MemoryStore) ListSessions(_ context.Context, eventDayID uuid.UUID) ([]Session, error) {
	return s.listSessions(eventDayID, lifecycle.CurrentOnly, true), nil
}

func (s *MemoryStore) ListSessionsLifecycle(_ context.Context, eventDayID uuid.UUID, visibility lifecycle.Visibility) ([]Session, error) {
	return s.listSessions(eventDayID, visibility, false), nil
}

func (s *MemoryStore) listSessions(eventDayID uuid.UUID, visibility lifecycle.Visibility, requireCurrentParents bool) []Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Session, 0)
	if requireCurrentParents {
		d, ok := s.days[eventDayID]
		if !ok || d.ArchivedAt != nil {
			return out
		}
		e, ok := s.byID[d.EventID]
		if !ok || e.ArchivedAt != nil {
			return out
		}
	}
	for _, sess := range s.sessions {
		if !visibility.Matches(sess.ArchivedAt != nil) {
			continue
		}
		if sess.EventDayID == eventDayID {
			out = append(out, sess)
		}
	}
	return out
}

func (s *MemoryStore) CreateSession(_ context.Context, sess Session) (Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sess.ID == uuid.Nil {
		sess.ID = uuid.New()
	}
	s.sessions[sess.ID] = sess
	return sess, nil
}

func (s *MemoryStore) UpdateSession(_ context.Context, sess Session) (Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	existing, ok := s.sessions[sess.ID]
	if !ok || existing.ArchivedAt != nil {
		return Session{}, ErrNotFound
	}
	s.sessions[sess.ID] = sess
	return sess, nil
}

func (s *MemoryStore) ArchiveSession(_ context.Context, id uuid.UUID, actorID *uuid.UUID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[id]
	if !ok {
		return ErrNotFound
	}
	if sess.ArchivedAt == nil {
		now := time.Now().UTC()
		sess.ArchivedAt = &now
		sess.ArchivedBy = actorID
		s.sessions[id] = sess
	}
	return nil
}

func (s *MemoryStore) RestoreSession(_ context.Context, id uuid.UUID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[id]
	if !ok {
		return ErrNotFound
	}
	if sess.ArchivedAt != nil {
		sess.ArchivedAt = nil
		sess.ArchivedBy = nil
		s.sessions[id] = sess
	}
	return nil
}
