package certificate

import (
	"context"
	"sync"
	"time"

	"github.com/google/uuid"
)

type record struct {
	cert Certificate
	pdf  []byte
}

type MemoryStore struct {
	mu   sync.Mutex
	byID map[uuid.UUID]record
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{byID: make(map[uuid.UUID]record)}
}

func (s *MemoryStore) Create(_ context.Context, c Certificate, pdf []byte) (Certificate, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c.ID == uuid.Nil {
		c.ID = uuid.New()
	}
	if c.IssuedAt.IsZero() {
		c.IssuedAt = time.Now().UTC()
	}
	for _, rec := range s.byID {
		if rec.cert.EventID == c.EventID && rec.cert.TicketID == c.TicketID && rec.cert.RevokedAt == nil {
			return Certificate{}, ErrConflict
		}
		if rec.cert.Serial == c.Serial {
			return Certificate{}, ErrConflict
		}
	}
	s.byID[c.ID] = record{cert: c, pdf: append([]byte(nil), pdf...)}
	return c, nil
}

func (s *MemoryStore) GetBySerial(_ context.Context, serial string) (Certificate, []byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, rec := range s.byID {
		if rec.cert.Serial == serial {
			return rec.cert, append([]byte(nil), rec.pdf...), nil
		}
	}
	return Certificate{}, nil, ErrNotFound
}

func (s *MemoryStore) GetActive(_ context.Context, eventID, ticketID uuid.UUID) (Certificate, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, rec := range s.byID {
		if rec.cert.EventID == eventID && rec.cert.TicketID == ticketID && rec.cert.RevokedAt == nil {
			return rec.cert, nil
		}
	}
	return Certificate{}, ErrNotFound
}

func (s *MemoryStore) ListByOwner(_ context.Context, ownerID uuid.UUID) ([]Certificate, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Certificate, 0)
	for _, rec := range s.byID {
		if rec.cert.OwnerID != nil && *rec.cert.OwnerID == ownerID {
			out = append(out, rec.cert)
		}
	}
	return out, nil
}

func (s *MemoryStore) ListByEvent(_ context.Context, eventID uuid.UUID) ([]Certificate, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Certificate, 0)
	for _, rec := range s.byID {
		if rec.cert.EventID == eventID {
			out = append(out, rec.cert)
		}
	}
	return out, nil
}

func (s *MemoryStore) Revoke(_ context.Context, serial string) (Certificate, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, rec := range s.byID {
		if rec.cert.Serial != serial {
			continue
		}
		if rec.cert.RevokedAt != nil {
			return rec.cert, nil
		}
		now := time.Now().UTC()
		rec.cert.RevokedAt = &now
		s.byID[id] = rec
		return rec.cert, nil
	}
	return Certificate{}, ErrNotFound
}
