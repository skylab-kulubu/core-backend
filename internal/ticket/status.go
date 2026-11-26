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

// FormsRejectionStore keeps Forms' decline of a Ticket's answer as a mark
// instead of a cancellation (Yusuf, K6 2026-11-25; PostgresStore,
// MemoryStore).
type FormsRejectionStore interface {
	// MarkFormsRejected marks the Ticket declined in Forms at at (its
	// answer's status declined); false when it is cancelled or marked
	// already.
	// A Ticket whose answer's form is not kept (bound before migration
	// 20261127110000) takes formID, the report's.
	MarkFormsRejected(ctx context.Context, id, formID uuid.UUID, at time.Time) (bool, error)
	// MarkFormResponsePending keeps that the answer responseID waits for
	// Forms' review again: its Tickets (not cancelled) lose their mark.
	MarkFormResponsePending(ctx context.Context, responseID uuid.UUID) error
	// KeepFormResponse keeps the accepted answer responseID of the form
	// formID on a Ticket it found or wrote, not cancelled, whose answer is
	// a forms answer (form_response_id set), and whose holder is the
	// answer's (userID the owner, or nil for a guest Ticket). True when it
	// did: the answer is the Ticket's own (written or reopened by it, or
	// accepted again), or the Ticket's answer is of the same form and no
	// longer accepted (declined, a correction asked, pending again), so the
	// newer answer takes it over. Its status is then accepted and its mark
	// cleared. Another form's answer (an extra form), or one while the
	// Ticket's answer stands, never takes it over.
	KeepFormResponse(ctx context.Context, id, formID, responseID uuid.UUID, userID *uuid.UUID) (bool, error)
}

// FormsStatusOf is a Ticket's Ticket.FormsStatus: "" for a Ticket no forms
// answer wrote, declined when Forms declined it (K6), else the answer's
// status as kept (tickets.form_response_status), accepted for an answer
// kept before that column.
func FormsStatusOf(fromForm bool, status *string, rejectedAt *time.Time) string {
	switch {
	case !fromForm:
		return ""
	case rejectedAt != nil:
		return string(FormResponseDeclined)
	case status != nil && *status != "":
		return *status
	}
	return string(FormResponseAccepted)
}

// CancelBlockStore reads which of the Tickets ids no cancellation may
// touch (PostgresStore, MemoryStore): one with a check-in, or of a
// Competition's current Entrant, as Cancel refuses them
// (ErrTicketCheckedIn, ErrTicketInCompetition). Only the blocked ones are
// in the answer.
type CancelBlockStore interface {
	CancelBlocked(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]bool, error)
}

// HolderMayCancel is HolderCancellable for a Ticket that no cancellation
// may touch (blocked: CancelBlockStore) too: what the ticket page's and the
// holder's list's cancellable say, so the button shows only where it works.
func HolderMayCancel(ev event.Event, t Ticket, now time.Time, blocked bool) bool {
	return !blocked && HolderCancellable(ev, t, now)
}

// cancelBlocked reads CancelBlockStore for one Ticket; a store that keeps
// no such thing blocks nothing.
func cancelBlocked(ctx context.Context, tickets any, id uuid.UUID) (bool, error) {
	store, ok := tickets.(CancelBlockStore)
	if !ok {
		return false, nil
	}
	blocked, err := store.CancelBlocked(ctx, []uuid.UUID{id})
	if err != nil {
		return false, err
	}
	return blocked[id], nil
}

// CancelBlocked is cancelBlocked for the ticket page.
func CancelBlocked(ctx context.Context, tickets Store, id uuid.UUID) (bool, error) {
	return cancelBlocked(ctx, tickets, id)
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
