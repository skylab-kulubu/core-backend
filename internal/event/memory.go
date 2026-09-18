package event

import (
	"context"
	"sync"
	"time"

	"github.com/google/uuid"
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
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Event, 0, len(s.byID))
	for _, e := range s.byID {
		if ownerTeam != "" && e.OwnerTeam != ownerTeam {
			continue
		}
		if activeOnly && !e.Active {
			continue
		}
		out = append(out, emptyGallery(e))
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
	if !ok {
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

func (s *MemoryStore) Delete(_ context.Context, id uuid.UUID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.byID[id]; !ok {
		return ErrNotFound
	}
	delete(s.byID, id)
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
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Day, 0)
	for _, d := range s.days {
		if d.EventID == eventID {
			out = append(out, d)
		}
	}
	return out, nil
}

func (s *MemoryStore) UpdateDay(_ context.Context, d Day) (Day, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.days[d.ID]; !ok {
		return Day{}, ErrNotFound
	}
	s.days[d.ID] = d
	return d, nil
}

func (s *MemoryStore) DeleteDay(_ context.Context, id uuid.UUID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.days[id]; !ok {
		return ErrNotFound
	}
	delete(s.days, id)
	return nil
}

func (s *MemoryStore) ListBySeason(_ context.Context, seasonID uuid.UUID) ([]Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Event, 0)
	for _, e := range s.byID {
		if e.SeasonID != nil && *e.SeasonID == seasonID {
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
	if !ok {
		return Session{}, ErrNotFound
	}
	return sess, nil
}

func (s *MemoryStore) ListSessions(_ context.Context, eventDayID uuid.UUID) ([]Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Session, 0)
	for _, sess := range s.sessions {
		if sess.EventDayID == eventDayID {
			out = append(out, sess)
		}
	}
	return out, nil
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
	if _, ok := s.sessions[sess.ID]; !ok {
		return Session{}, ErrNotFound
	}
	s.sessions[sess.ID] = sess
	return sess, nil
}

func (s *MemoryStore) DeleteSession(_ context.Context, id uuid.UUID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.sessions[id]; !ok {
		return ErrNotFound
	}
	delete(s.sessions, id)
	return nil
}
