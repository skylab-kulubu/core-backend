package ticket

import (
	"context"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

type MemoryStore struct {
	mu       sync.Mutex
	byID     map[uuid.UUID]Ticket
	checkIns map[uuid.UUID]CheckIn
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		byID:     make(map[uuid.UUID]Ticket),
		checkIns: make(map[uuid.UUID]CheckIn),
	}
}

func (s *MemoryStore) withCheckInsLocked(t Ticket) Ticket {
	out := make([]CheckIn, 0)
	for _, c := range s.checkIns {
		if c.TicketID == t.ID {
			out = append(out, c)
		}
	}
	t.CheckIns = out
	return t
}

func (s *MemoryStore) Create(_ context.Context, t Ticket) (Ticket, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if t.ID == uuid.Nil {
		t.ID = uuid.New()
	}
	now := time.Now().UTC()
	t.CreatedAt = now
	t.UpdatedAt = now
	if t.Status == "" {
		t.Status = StatusActive
	}
	if t.CheckIns == nil {
		t.CheckIns = []CheckIn{}
	}
	// As the database: a fresh access key and code, unique, QR version 1.
	for {
		key, err := NewAccessKey()
		if err != nil {
			return Ticket{}, err
		}
		code, err := NewCode()
		if err != nil {
			return Ticket{}, err
		}
		if !s.takenLocked(key, code) {
			t.AccessKey, t.Code, t.QRVersion = key, FormatCode(code), 1
			break
		}
	}
	s.byID[t.ID] = t
	return t, nil
}

func (s *MemoryStore) takenLocked(key, code string) bool {
	for _, other := range s.byID {
		if other.AccessKey == key || storedCode(other.Code) == code {
			return true
		}
	}
	return false
}

func (s *MemoryStore) GetByAccessKey(_ context.Context, key string) (Ticket, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !ValidAccessKey(key) {
		return Ticket{}, ErrNotFound
	}
	for _, t := range s.byID {
		if t.AccessKey == key {
			return s.withCheckInsLocked(t), nil
		}
	}
	return Ticket{}, ErrNotFound
}

func (s *MemoryStore) Get(_ context.Context, id uuid.UUID) (Ticket, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.byID[id]
	if !ok {
		return Ticket{}, ErrNotFound
	}
	return s.withCheckInsLocked(t), nil
}

func (s *MemoryStore) ListByOwner(_ context.Context, ownerID uuid.UUID) ([]Ticket, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Ticket, 0)
	for _, t := range s.byID {
		if t.OwnerID != nil && *t.OwnerID == ownerID {
			out = append(out, s.withCheckInsLocked(t))
		}
	}
	return out, nil
}

// ListByEvent is the Event's active Tickets, as the database's.
func (s *MemoryStore) ListByEvent(ctx context.Context, eventID uuid.UUID) ([]Ticket, error) {
	return s.ListByEventStatus(ctx, eventID, []Status{StatusActive})
}

func (s *MemoryStore) ListByEventStatus(_ context.Context, eventID uuid.UUID, statuses []Status) ([]Ticket, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Ticket, 0)
	for _, t := range s.byID {
		if t.EventID == eventID && (len(statuses) == 0 || slices.Contains(statuses, t.StatusOf())) {
			out = append(out, s.withCheckInsLocked(t))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}

// Cancel is the database's, without the Competitions (the memory store
// keeps none).
func (s *MemoryStore) Cancel(_ context.Context, id uuid.UUID, by CancelledBy, reason string) (Ticket, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.byID[id]
	if !ok {
		return Ticket{}, false, ErrNotFound
	}
	if t.StatusOf() == StatusCancelled {
		return s.withCheckInsLocked(t), false, nil
	}
	for _, c := range s.checkIns {
		if c.TicketID == id {
			return Ticket{}, false, ErrTicketCheckedIn
		}
	}
	now := time.Now().UTC()
	t.Status, t.StatusChangedAt, t.WaitlistedAt = StatusCancelled, &now, nil
	t.CancelledAt, t.CancelledBy, t.CancelReason = &now, by, strings.TrimSpace(reason)
	t.UpdatedAt = now
	s.byID[id] = t
	return s.withCheckInsLocked(t), true, nil
}

// Reopen is the database's: a new key and the next QR version.
func (s *MemoryStore) Reopen(_ context.Context, id uuid.UUID) (Ticket, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.byID[id]
	if !ok {
		return Ticket{}, false, ErrNotFound
	}
	if t.StatusOf() != StatusCancelled {
		return s.withCheckInsLocked(t), false, nil
	}
	key, err := NewAccessKey()
	if err != nil {
		return Ticket{}, false, err
	}
	now := time.Now().UTC()
	t.Status, t.StatusChangedAt, t.WaitlistedAt = StatusActive, &now, nil
	t.CancelledAt, t.CancelledBy, t.CancelReason = nil, "", ""
	if t.AccessKey != "" {
		t.AccessKey = key
	}
	t.QRVersion++
	t.UpdatedAt = now
	s.byID[id] = t
	return s.withCheckInsLocked(t), true, nil
}

func (s *MemoryStore) ExistsOwnerEvent(_ context.Context, ownerID, eventID uuid.UUID) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, t := range s.byID {
		if t.OwnerID != nil && *t.OwnerID == ownerID && t.EventID == eventID {
			return true, nil
		}
	}
	return false, nil
}

func guestEmailMatch(stored, want string) bool {
	return strings.ToLower(strings.TrimSpace(stored)) == strings.ToLower(strings.TrimSpace(want))
}

func (s *MemoryStore) ExistsGuestEvent(_ context.Context, email string, eventID uuid.UUID) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, t := range s.byID {
		if t.TicketType == Guest && guestEmailMatch(t.GuestEmail, email) && t.EventID == eventID {
			return true, nil
		}
	}
	return false, nil
}

func (s *MemoryStore) GetByGuestEvent(_ context.Context, email string, eventID uuid.UUID) (Ticket, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, t := range s.byID {
		if t.TicketType == Guest && guestEmailMatch(t.GuestEmail, email) && t.EventID == eventID {
			return s.withCheckInsLocked(t), nil
		}
	}
	return Ticket{}, ErrNotFound
}

func (s *MemoryStore) Update(_ context.Context, t Ticket) (Ticket, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	existing, ok := s.byID[t.ID]
	if !ok {
		return Ticket{}, ErrNotFound
	}
	t.CreatedAt = existing.CreatedAt
	t.UpdatedAt = time.Now().UTC()
	// Update writes the guest's details only, as the database's.
	t.AccessKey, t.Code, t.QRVersion = existing.AccessKey, existing.Code, existing.QRVersion
	t.Status, t.StatusChangedAt, t.WaitlistedAt = existing.Status, existing.StatusChangedAt, existing.WaitlistedAt
	t.CancelledAt, t.CancelledBy, t.CancelReason = existing.CancelledAt, existing.CancelledBy, existing.CancelReason
	if t.CheckIns == nil {
		t.CheckIns = existing.CheckIns
	}
	s.byID[t.ID] = t
	return s.withCheckInsLocked(t), nil
}

func (s *MemoryStore) GetByOwnerEvent(_ context.Context, ownerID, eventID uuid.UUID) (Ticket, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, t := range s.byID {
		if t.OwnerID != nil && *t.OwnerID == ownerID && t.EventID == eventID {
			return s.withCheckInsLocked(t), nil
		}
	}
	return Ticket{}, ErrNotFound
}

func (s *MemoryStore) ListByGuestEmail(_ context.Context, email string) ([]Ticket, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	want := strings.ToLower(strings.TrimSpace(email))
	out := make([]Ticket, 0)
	for _, t := range s.byID {
		if t.TicketType == Guest && strings.ToLower(t.GuestEmail) == want {
			out = append(out, s.withCheckInsLocked(t))
		}
	}
	return out, nil
}

func (s *MemoryStore) AddCheckIn(_ context.Context, c CheckIn) (CheckIn, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.byID[c.TicketID]
	if !ok {
		return CheckIn{}, ErrNotFound
	}
	if refused := inactiveErr(t); refused != nil {
		return CheckIn{}, refused
	}
	if c.ID == uuid.Nil {
		c.ID = uuid.New()
	}
	c.CreatedAt = time.Now().UTC()
	s.checkIns[c.ID] = c
	return c, nil
}

func (s *MemoryStore) RemoveCheckIn(_ context.Context, ticketID, sessionID uuid.UUID) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, c := range s.checkIns {
		if c.TicketID == ticketID && c.SessionID == sessionID {
			delete(s.checkIns, id)
			return true, nil
		}
	}
	return false, nil
}

func (s *MemoryStore) CheckInByID(_ context.Context, id uuid.UUID) (CheckIn, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.checkIns[id]
	if !ok {
		return CheckIn{}, ErrNotFound
	}
	return c, nil
}

func (s *MemoryStore) RemoveCheckInByID(_ context.Context, id uuid.UUID) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.checkIns[id]; !ok {
		return false, nil
	}
	delete(s.checkIns, id)
	return true, nil
}

func (s *MemoryStore) HasCheckIn(_ context.Context, ticketID, sessionID uuid.UUID) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.checkIns {
		if c.TicketID == ticketID && c.SessionID == sessionID {
			return true, nil
		}
	}
	return false, nil
}
