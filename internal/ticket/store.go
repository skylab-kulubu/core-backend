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
)

type Store interface {
	Create(ctx context.Context, t Ticket) (Ticket, error)
	Get(ctx context.Context, id uuid.UUID) (Ticket, error)
	ListByOwner(ctx context.Context, ownerID uuid.UUID) ([]Ticket, error)
	ExistsOwnerEvent(ctx context.Context, ownerID, eventID uuid.UUID) (bool, error)
	ExistsGuestEvent(ctx context.Context, email string, eventID uuid.UUID) (bool, error)
	AddCheckIn(ctx context.Context, c CheckIn) (CheckIn, error)
	HasCheckIn(ctx context.Context, ticketID, eventDayID uuid.UUID) (bool, error)
}
