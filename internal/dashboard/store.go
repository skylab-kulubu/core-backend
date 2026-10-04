package dashboard

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/media"
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
	// Accounts reads what core holds of each of ids: whether core may still
	// show them (Account.Blocked) and the names it stores. A person core has
	// neither a row nor a deletion marker for is left out.
	Accounts(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]Account, error)
}

// Account is what core holds of a person the Members section may name.
type Account struct {
	// Blocked is true for a person core may no longer show: one with an
	// account deletion request (the erasure marker, which outlives the row)
	// or whose row is no longer active. It is user.PostgresStore's
	// AttributionState rule, for many people at once.
	Blocked bool
	// Stored is true when core has a row for the person; FirstName and
	// LastName are then the names it stores, which core's other people
	// reads answer in place of Keycloak's.
	Stored    bool
	FirstName string
	LastName  string
	// ProfilePictureKey is the object key of the person's profile picture,
	// read as user.PostgresStore reads it (the key of the Media the profile
	// links, or the address stored for a picture with no Media), and
	// ProfilePicture that Media, which the picture's sizes are built from.
	// Empty and nil for a profile without a picture.
	ProfilePictureKey string
	ProfilePicture    *media.LinkedImage
}

// accounts is what MemoryStore reads people from: user.MemoryStore.
type accounts interface {
	Get(ctx context.Context, id uuid.UUID) (user.User, error)
	AttributionState(ctx context.Context, id uuid.UUID) (user.AttributionState, error)
}

// MemoryStore answers Store from the in-memory ticket and user stores, one
// Event at a time: for development and tests, where nothing is large.
type MemoryStore struct {
	tickets ticket.Store
	users   accounts
}

// NewMemoryStore reads tickets and, when users is not nil, account states.
func NewMemoryStore(tickets ticket.Store, users accounts) *MemoryStore {
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

// Accounts follows the Postgres rule: a deletion marker blocks, and so does
// a row that is no longer active (user.MemoryStore.AttributionState).
func (s *MemoryStore) Accounts(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]Account, error) {
	out := make(map[uuid.UUID]Account)
	if s.users == nil {
		return out, nil
	}
	for _, id := range ids {
		var a Account
		row, err := s.users.Get(ctx, id)
		switch {
		case err == nil:
			a.Stored, a.FirstName, a.LastName = true, row.FirstName, row.LastName
			a.ProfilePictureKey, a.ProfilePicture = row.ProfilePictureURL, row.ProfilePicture
		case !errors.Is(err, user.ErrNotFound):
			return nil, err
		}
		state, err := s.users.AttributionState(ctx, id)
		if err != nil {
			return nil, err
		}
		a.Blocked = state == user.AttributionBlocked
		if a.Stored || a.Blocked {
			out[id] = a
		}
	}
	return out, nil
}
