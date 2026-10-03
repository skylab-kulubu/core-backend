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
}

type DoorStore interface {
	SearchDoorTickets(ctx context.Context, eventID uuid.UUID, query string, exact bool, limit int) ([]DoorTicketIdentity, error)
	DoorTicketsByOwners(ctx context.Context, eventID uuid.UUID, ownerIDs []uuid.UUID) ([]DoorTicketIdentity, error)
	DoorSessionActivity(ctx context.Context, sessionID uuid.UUID, limit int) (DoorActivity, error)
}
