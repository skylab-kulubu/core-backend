package ticket

import (
	"context"
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
	if t.CheckIns == nil {
		t.CheckIns = []CheckIn{}
	}
	s.byID[t.ID] = t
	return t, nil
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

func (s *MemoryStore) ListByEvent(_ context.Context, eventID uuid.UUID) ([]Ticket, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Ticket, 0)
	for _, t := range s.byID {
		if t.EventID == eventID {
			out = append(out, s.withCheckInsLocked(t))
		}
	}
	return out, nil
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

func (s *MemoryStore) ExistsGuestEvent(_ context.Context, email string, eventID uuid.UUID) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, t := range s.byID {
		if t.TicketType == Guest && t.GuestEmail == email && t.EventID == eventID {
			return true, nil
		}
	}
	return false, nil
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
	if _, ok := s.byID[c.TicketID]; !ok {
		return CheckIn{}, ErrNotFound
	}
	if c.ID == uuid.Nil {
		c.ID = uuid.New()
	}
	c.CreatedAt = time.Now().UTC()
	s.checkIns[c.ID] = c
	return c, nil
}

func (s *MemoryStore) HasCheckIn(_ context.Context, ticketID, eventDayID uuid.UUID) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.checkIns {
		if c.TicketID == ticketID && c.EventDayID == eventDayID {
			return true, nil
		}
	}
	return false, nil
}
