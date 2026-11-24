package ticket

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/event"
)

// An Event's capacity and waiting list (kontenjan-iptal-hatirlatma 06, 07;
// docs/event-capacity.md).

// ErrEventFull is a registration a full Event does not take: its waiting
// list is off, or the caller did not ask to join it (409 event_full). An
// *EventFullError says which.
var ErrEventFull = errors.New("ticket: event full")

// EventFullError is ErrEventFull with the Event's waiting list switch, which
// the refusal tells the client (it offers to join while it is on).
type EventFullError struct {
	WaitlistEnabled bool
}

func (e *EventFullError) Error() string { return ErrEventFull.Error() }

func (e *EventFullError) Is(target error) bool { return target == ErrEventFull }

// Admission is how a Ticket's write meets its Event's capacity.
type Admission struct {
	// Over writes the Ticket active past the capacity: an operator adding a
	// person from the panel (Yusuf, 2026-11-24), who is told so
	// (Ticket.OverCapacity).
	Over bool
	// Waitlist puts the Ticket on the waiting list when the Event is full
	// and its waiting list is on: a member or guest who asked to join it
	// (joinWaitlist), and an accepted forms answer. Without it, or with the
	// waiting list off, a full Event is ErrEventFull.
	Waitlist bool
	// FormResponse is the forms answer whose accepted report writes or
	// reopens the Ticket (docs/form-response-tickets.md): kept on the
	// Ticket, so that the answer's decline cancels it. uuid.Nil for any
	// other way: a Ticket it writes keeps none, one it reopens loses it.
	FormResponse uuid.UUID
	// Quiet queues no mail for the write: an attendee import whose
	// operator turned its mail off (docs/attendee-import.md). The Ticket
	// is written as without it; its mail can be sent later from the
	// panel.
	Quiet bool
}

// admit is the status a Ticket written under a, on an Event of c, takes,
// and whether it goes past the capacity.
func admit(c event.Capacity, a Admission) (Status, bool, error) {
	switch {
	case !c.Full():
		return StatusActive, false, nil
	case a.Over:
		return StatusActive, true, nil
	case a.Waitlist && c.Waitlist:
		return StatusWaitlisted, false, nil
	}
	return "", false, &EventFullError{WaitlistEnabled: c.Waitlist}
}

// overCapacity reports whether an Event of c has more active Tickets than
// its capacity once one more is active.
func overCapacity(c event.Capacity) bool {
	return c.Capacity > 0 && c.Active+1 > c.Capacity
}

// CapacityStore writes Tickets under their Event's capacity (PostgresStore,
// MemoryStore): each write holds the Event while it counts and writes.
type CapacityStore interface {
	// CreateAdmitted writes t as Create does, active or waitlisted as a
	// lets (admit); Ticket.OverCapacity says it went past the capacity.
	CreateAdmitted(ctx context.Context, t Ticket, a Admission) (Ticket, error)
	// ReopenAdmitted reopens a cancelled Ticket as Reopen does, active or
	// waitlisted as a lets. A Ticket that is not cancelled is answered as
	// it is, changed false.
	ReopenAdmitted(ctx context.Context, id uuid.UUID, a Admission) (Ticket, bool, error)
	// Promote makes a waitlisted Ticket active, past the capacity if it
	// must (an operator's "Öne al"); its mail is queued. A Ticket that is
	// not waitlisted is answered as it is, changed false.
	Promote(ctx context.Context, id uuid.UUID) (Ticket, bool, error)
	// Capacity is the Event's capacity now, unlocked, for an answer that
	// writes nothing.
	Capacity(ctx context.Context, eventID uuid.UUID) (event.Capacity, error)
}

// capacityStore is the Tickets' CapacityStore: ErrStatusUnavailable
// without one.
func (s *service) capacityStore() (CapacityStore, error) {
	store, ok := s.tickets.(CapacityStore)
	if !ok {
		return nil, ErrStatusUnavailable
	}
	return store, nil
}
