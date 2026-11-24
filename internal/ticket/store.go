package ticket

import (
	"context"
	"errors"

	"github.com/google/uuid"
)

var (
	ErrNotFound  = errors.New("ticket: not found")
	ErrForbidden = errors.New("ticket: forbidden")
	ErrInvalid   = errors.New("ticket: invalid")
	ErrConflict  = errors.New("ticket: conflict")
	ErrAmbiguous = errors.New("ticket: ambiguous match")

	// Guest check-in's door QR (docs/guest-self-check-in.md).
	ErrDoorQRRequired = errors.New("ticket: door qr required")
	ErrDoorQRInvalid  = errors.New("ticket: door qr invalid")
	ErrDoorQRExpired  = errors.New("ticket: door qr expired")
	ErrDoorQRUsedUp   = errors.New("ticket: door qr used up")
	// ErrSessionClosed is a Session that is cancelled, or not within its
	// time window (with the door QR's grace) now.
	ErrSessionClosed = errors.New("ticket: session closed")
)

type Store interface {
	Create(ctx context.Context, t Ticket) (Ticket, error)
	Get(ctx context.Context, id uuid.UUID) (Ticket, error)
	ListByOwner(ctx context.Context, ownerID uuid.UUID) ([]Ticket, error)
	ListByEvent(ctx context.Context, eventID uuid.UUID) ([]Ticket, error)
	ExistsOwnerEvent(ctx context.Context, ownerID, eventID uuid.UUID) (bool, error)
	ExistsGuestEvent(ctx context.Context, email string, eventID uuid.UUID) (bool, error)
	GetByGuestEvent(ctx context.Context, email string, eventID uuid.UUID) (Ticket, error)
	GetByOwnerEvent(ctx context.Context, ownerID, eventID uuid.UUID) (Ticket, error)
	ListByGuestEmail(ctx context.Context, email string) ([]Ticket, error)
	Update(ctx context.Context, t Ticket) (Ticket, error)
	AddCheckIn(ctx context.Context, c CheckIn) (CheckIn, error)
	HasCheckIn(ctx context.Context, ticketID, sessionID uuid.UUID) (bool, error)
	// RemoveCheckIn deletes the check-in ticketID has at sessionID; false
	// when it has none there.
	RemoveCheckIn(ctx context.Context, ticketID, sessionID uuid.UUID) (bool, error)
	// CheckInByID is the check-in with id: ErrNotFound when none has it.
	CheckInByID(ctx context.Context, id uuid.UUID) (CheckIn, error)
	// RemoveCheckInByID deletes the check-in with id; false when none has
	// it.
	RemoveCheckInByID(ctx context.Context, id uuid.UUID) (bool, error)
	// GetByAccessKey is the Ticket the ticket page's key opens
	// (docs/ticket-page.md): ErrNotFound when none has it.
	GetByAccessKey(ctx context.Context, key string) (Ticket, error)
	VenueEntryStore
}

// VenueEntryStore keeps the Venue entries (docs/venue-entry.md). AddCheckIn
// writes the day's entry of a check-in that carries one, and RemoveCheckIn
// and RemoveCheckInByID take back the entry the check-in wrote, unless the
// Ticket has another check-in that day (which then holds it).
type VenueEntryStore interface {
	// AddVenueEntry writes e when its Ticket is active and has no entry on
	// e.EntryDate: ErrConflict when it has one, the door's refusal when the
	// Ticket is cancelled or waitlisted.
	AddVenueEntry(ctx context.Context, e VenueEntry) (VenueEntry, error)
	// VenueEntryByID is the entry with id: ErrNotFound when none.
	VenueEntryByID(ctx context.Context, id uuid.UUID) (VenueEntry, error)
	// RemoveVenueEntry deletes the entry with id; false when none has it.
	// The day's check-ins stay.
	RemoveVenueEntry(ctx context.Context, id uuid.UUID) (bool, error)
	// VenueEntriesOn are the Event's entries on date (YYYY-MM-DD), newest
	// first, each with the name the door shows (a member's is left empty
	// where the store does not know it).
	VenueEntriesOn(ctx context.Context, eventID uuid.UUID, date string) ([]DoorEntry, error)
	// VenueEntryDays counts the Event's entries by day and source, the
	// days in order.
	VenueEntryDays(ctx context.Context, eventID uuid.UUID) ([]EntryDay, error)
}

type DoorStore interface {
	SearchDoorTickets(ctx context.Context, eventID uuid.UUID, query string, exact bool, limit int) ([]DoorTicketIdentity, error)
	DoorTicketsByOwners(ctx context.Context, eventID uuid.UUID, ownerIDs []uuid.UUID) ([]DoorTicketIdentity, error)
	DoorSessionActivity(ctx context.Context, sessionID uuid.UUID, limit int) (DoorActivity, error)
}
