package dashboard

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// TicketCounts are one Event's applications: every Ticket, Member apply
// (REGISTERED) and the rest (Guest apply and Walk-in), and the Tickets with
// at least one check-in.
type TicketCounts struct {
	Applications int
	Members      int
	Guests       int
	CheckedIn    int
}

// Store reads the aggregates the summary is made of. Every method takes the
// Events the caller may see and answers for those alone.
type Store interface {
	// TicketCounts counts the Tickets of each Event. An Event without
	// Tickets may be left out.
	TicketCounts(ctx context.Context, eventIDs []uuid.UUID) (map[uuid.UUID]TicketCounts, error)
	// DailyApplications counts the Tickets of each Event created at or
	// after since, by calendar day in loc ("2006-01-02").
	DailyApplications(ctx context.Context, eventIDs []uuid.UUID, since time.Time, loc *time.Location) (map[uuid.UUID]map[string]int, error)
	// BlockedAccounts names the people among ids whose account core may no
	// longer show: one with an account deletion request (the erasure
	// marker, which outlives the row) or whose row is no longer active.
	// Someone core has no row for is not blocked.
	BlockedAccounts(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]bool, error)
}

// attributions is what MemoryStore reads an account's state from:
// user.MemoryStore's rule for a person core may still name.
type attributions interface {
	AttributionState(ctx context.Context, id uuid.UUID) (user.AttributionState, error)
}

// MemoryStore answers Store from the in-memory ticket and user stores, one
// Event at a time: for development and tests, where nothing is large.
type MemoryStore struct {
	tickets ticket.Store
	users   attributions
}

// NewMemoryStore reads tickets and, when users is not nil, account states.
func NewMemoryStore(tickets ticket.Store, users attributions) *MemoryStore {
	return &MemoryStore{tickets: tickets, users: users}
}

func (s *MemoryStore) TicketCounts(ctx context.Context, eventIDs []uuid.UUID) (map[uuid.UUID]TicketCounts, error) {
	out := make(map[uuid.UUID]TicketCounts, len(eventIDs))
	for _, id := range eventIDs {
		listed, err := s.tickets.ListByEvent(ctx, id)
		if err != nil {
			return nil, err
		}
		var counts TicketCounts
		for _, t := range listed {
			counts.Applications++
			if t.TicketType == ticket.Registered {
				counts.Members++
			} else {
				counts.Guests++
			}
			if len(t.CheckIns) > 0 {
				counts.CheckedIn++
			}
		}
		if counts.Applications > 0 {
			out[id] = counts
		}
	}
	return out, nil
}

func (s *MemoryStore) DailyApplications(ctx context.Context, eventIDs []uuid.UUID, since time.Time, loc *time.Location) (map[uuid.UUID]map[string]int, error) {
	out := make(map[uuid.UUID]map[string]int)
	for _, id := range eventIDs {
		listed, err := s.tickets.ListByEvent(ctx, id)
		if err != nil {
			return nil, err
		}
		for _, t := range listed {
			if t.CreatedAt.Before(since) {
				continue
			}
			if out[id] == nil {
				out[id] = make(map[string]int)
			}
			out[id][t.CreatedAt.In(loc).Format(time.DateOnly)]++
		}
	}
	return out, nil
}

func (s *MemoryStore) BlockedAccounts(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]bool, error) {
	out := make(map[uuid.UUID]bool)
	if s.users == nil {
		return out, nil
	}
	for _, id := range ids {
		state, err := s.users.AttributionState(ctx, id)
		if err != nil {
			return nil, err
		}
		if state == user.AttributionBlocked {
			out[id] = true
		}
	}
	return out, nil
}
