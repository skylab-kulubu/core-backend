package ticket

import (
	"context"
	"errors"

	"github.com/google/uuid"
)

// A Ticket's status and an operator's cancellation
// (docs/ticket-cancellation.md).
var (
	// ErrTicketCancelled and ErrTicketWaitlisted are a Ticket that is not
	// active: the door refuses it (409 ticket_cancelled, ticket_waitlisted).
	ErrTicketCancelled  = errors.New("ticket: cancelled")
	ErrTicketWaitlisted = errors.New("ticket: waitlisted")
	// ErrTicketCheckedIn is a Ticket with a check-in: it is not cancelled
	// (undo the check-in first).
	ErrTicketCheckedIn = errors.New("ticket: checked in")
	// ErrTicketInCompetition is a Ticket of a Competition's current
	// Entrant (ADR 0068): it is not cancelled.
	ErrTicketInCompetition = errors.New("ticket: in competition")
	// ErrStatusUnavailable is a store that keeps no status.
	ErrStatusUnavailable = errors.New("ticket: status unavailable")
)

// StatusStore cancels Tickets and reopens cancelled ones (PostgresStore,
// MemoryStore).
type StatusStore interface {
	ListByEventStatus(ctx context.Context, eventID uuid.UUID, statuses []Status) ([]Ticket, error)
	Cancel(ctx context.Context, id uuid.UUID, by CancelledBy, reason string) (Ticket, bool, error)
	Reopen(ctx context.Context, id uuid.UUID) (Ticket, bool, error)
}

// inactiveErr is the door's refusal of t: nil for an active Ticket.
func inactiveErr(t Ticket) error {
	switch t.StatusOf() {
	case StatusCancelled:
		return ErrTicketCancelled
	case StatusWaitlisted:
		return ErrTicketWaitlisted
	}
	return nil
}

// statusErr is the door's refusal of a Ticket of status: nil for an active
// one.
func statusErr(status Status) error {
	return inactiveErr(Ticket{Status: status})
}
