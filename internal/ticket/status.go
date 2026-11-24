package ticket

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/event"
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
	// ErrCancelClosed is a holder's own cancellation once it is too late
	// (409 ticket_cancel_closed): an active Ticket once its Event has
	// started, any Ticket once the Event has ended or while it is cancelled.
	ErrCancelClosed = errors.New("ticket: cancellation closed")
)

// StatusStore cancels Tickets and reopens cancelled ones (PostgresStore,
// MemoryStore).
type StatusStore interface {
	ListByEventStatus(ctx context.Context, eventID uuid.UUID, statuses []Status) ([]Ticket, error)
	Cancel(ctx context.Context, id uuid.UUID, by CancelledBy, reason string) (Ticket, bool, error)
	Reopen(ctx context.Context, id uuid.UUID) (Ticket, bool, error)
}

// CancelMailStore cancels a Ticket as StatusStore.Cancel does and, when
// tell, queues its holder's "Biletin iptal edildi" mail
// (event.ticket-cancelled) in the same transaction while the status mails
// are on (TICKET_STATUS_MAIL, docs/ticket-status-mail.md). Cancel tells
// nobody.
type CancelMailStore interface {
	CancelTelling(ctx context.Context, id uuid.UUID, by CancelledBy, reason string, tell bool) (Ticket, bool, error)
}

// FormResponseStore finds the Tickets a forms answer wrote or reopened
// (tickets.form_response_id, docs/form-response-tickets.md), whatever their
// status.
type FormResponseStore interface {
	FormResponseTickets(ctx context.Context, responseID uuid.UUID) ([]Ticket, error)
}

// HolderCancellable reports whether t's holder may cancel it themselves at
// now (kontenjan-iptal-hatirlatma 03; Yusuf, 2026-11-24): an active Ticket
// until its Event starts, a waitlisted one (leaving the list) until the
// Event ends. Never a cancelled Ticket, nor one of a cancelled Event. An
// undated Event never starts.
func HolderCancellable(ev event.Event, t Ticket, now time.Time) bool {
	if ev.Status == event.StatusCancelled || ev.EndedAt(now) {
		return false
	}
	switch t.StatusOf() {
	case StatusWaitlisted:
		return true
	case StatusActive:
		return !ev.StartedAt(now)
	}
	return false
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
