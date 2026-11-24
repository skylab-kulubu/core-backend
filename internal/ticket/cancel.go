package ticket

import (
	"context"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/teamaudit"
)

// An operator cancelling a Ticket and taking the cancellation back
// (POST /v1/tickets/{id}/cancel, /restore; docs/ticket-cancellation.md).

// statusStore is the Tickets' StatusStore: ErrStatusUnavailable without one.
func (s *service) statusStore() (StatusStore, error) {
	store, ok := s.tickets.(StatusStore)
	if !ok {
		return nil, ErrStatusUnavailable
	}
	return store, nil
}

// ticketOperator reports whether p may cancel and restore ev's Tickets:
// exactly who may edit the Event (Yusuf, 2026-11-24): Privileged
// event:manage, the owning team's Leaders, and the members of a team whose
// members edit its Events (GECEKODU). A Privileged reader (DK) and an
// ordinary member may not.
func (s *service) ticketOperator(p authz.Principal, ev event.Event) bool {
	return p.ID != "" && p.Product == "" &&
		s.authz.Allow(p, authz.Resource{Type: authz.TypeEvent, OwnerTeam: ev.OwnerTeam}, authz.Update)
}

// privilegedOperator reports whether p cancels as Privileged (the audit
// row's basis): ticket:manage or event:manage, not a Leader.
func (s *service) privilegedOperator(p authz.Principal) bool {
	return s.authz.Allow(p, authz.Resource{Type: authz.TypeTicket}, authz.Assign) ||
		s.authz.Allow(p, authz.Resource{Type: authz.TypeEvent}, authz.Assign)
}

// beginOperatorAudit opens the audit row of a cancellation or restoration:
// privileged, else the Leader, else the Event's editor (the basis
// event_editor, through the Event's team).
func (s *service) beginOperatorAudit(ctx context.Context, p authz.Principal, ev event.Event, action teamaudit.Action, ticketID uuid.UUID) (*auditRecord, error) {
	return s.beginTicketAuditBasis(ctx, p, ev, action, ticketID, s.privilegedOperator(p), true)
}

// operatedTicket is the Ticket id names and its Event, for an operator of
// its Event (ticketOperator): a draft p may not see is ErrNotFound.
func (s *service) operatedTicket(ctx context.Context, p authz.Principal, id uuid.UUID) (Ticket, event.Event, error) {
	t, err := s.tickets.Get(ctx, id)
	if err != nil {
		return Ticket{}, event.Event{}, err
	}
	ev, err := s.eventOf(ctx, t.EventID)
	if err != nil {
		return Ticket{}, event.Event{}, err
	}
	if err := s.seen(&p, ev); err != nil {
		return Ticket{}, event.Event{}, err
	}
	if !s.ticketOperator(p, ev) {
		return Ticket{}, event.Event{}, ErrForbidden
	}
	return t, ev, nil
}

// CancelTicket cancels the Ticket for an operator: until the Event ends
// (ErrEventEnded), never one with a check-in (ErrTicketCheckedIn) or of a
// Competition's Entrant (ErrTicketInCompetition). A Ticket already
// cancelled is answered as it is, with no audit row. reason is optional, at
// most MaxCancelReason characters.
func (s *service) CancelTicket(ctx context.Context, p authz.Principal, id uuid.UUID, reason string) (Ticket, error) {
	reason = strings.TrimSpace(reason)
	if utf8.RuneCountInString(reason) > MaxCancelReason {
		return Ticket{}, ErrInvalid
	}
	store, err := s.statusStore()
	if err != nil {
		return Ticket{}, err
	}
	t, ev, err := s.operatedTicket(ctx, p, id)
	if err != nil {
		return Ticket{}, err
	}
	if t.StatusOf() == StatusCancelled {
		return s.withEvent(ctx, t), nil
	}
	if ev.EndedAt(time.Now()) {
		return Ticket{}, ErrEventEnded
	}
	record, err := s.beginOperatorAudit(ctx, p, ev, teamaudit.ActionTicketCancel, id)
	if err != nil {
		return Ticket{}, err
	}
	cancelled, _, err := store.Cancel(ctx, id, CancelledByOperator, reason)
	s.finishTicketAudit(ctx, record, err, 0)
	if err != nil {
		return Ticket{}, err
	}
	return s.withEvent(ctx, cancelled), nil
}

// RestoreTicket takes an operator's (or anybody's) cancellation back: the
// Ticket is active again with a new link and QR, and its "Biletin hazır"
// mail goes again (while the Notifications are on). Until the Event ends;
// an active Ticket is answered as it is, a waitlisted one is ErrConflict
// (it is promoted, not restored).
func (s *service) RestoreTicket(ctx context.Context, p authz.Principal, id uuid.UUID) (Ticket, error) {
	store, err := s.statusStore()
	if err != nil {
		return Ticket{}, err
	}
	t, ev, err := s.operatedTicket(ctx, p, id)
	if err != nil {
		return Ticket{}, err
	}
	switch t.StatusOf() {
	case StatusActive:
		return s.withEvent(ctx, t), nil
	case StatusWaitlisted:
		return Ticket{}, ErrTicketWaitlisted
	}
	if ev.EndedAt(time.Now()) {
		return Ticket{}, ErrEventEnded
	}
	record, err := s.beginOperatorAudit(ctx, p, ev, teamaudit.ActionTicketRestore, id)
	if err != nil {
		return Ticket{}, err
	}
	restored, _, err := store.Reopen(ctx, id)
	s.finishTicketAudit(ctx, record, err, 0)
	if err != nil {
		return Ticket{}, err
	}
	return s.withEvent(ctx, restored), nil
}

// reopenIfCancelled reopens t when it is cancelled, for its holder
// registering again (Member apply, Apply-for-other, Guest apply, the forms
// report), under the Event's capacity (a): reopened reports it. A Ticket of
// any other status is left.
func (s *service) reopenIfCancelled(ctx context.Context, t Ticket, a Admission) (Ticket, bool, error) {
	if t.StatusOf() != StatusCancelled {
		return t, false, nil
	}
	var (
		reopened Ticket
		changed  bool
		err      error
	)
	if store, ok := s.tickets.(CapacityStore); ok {
		reopened, changed, err = store.ReopenAdmitted(ctx, t.ID, a)
	} else {
		var store StatusStore
		if store, err = s.statusStore(); err != nil {
			return Ticket{}, false, err
		}
		reopened, changed, err = store.Reopen(ctx, t.ID)
	}
	if err != nil {
		return Ticket{}, false, err
	}
	if !changed {
		// Reopened by another request since t was read: as one already
		// there.
		return reopened, false, nil
	}
	return reopened, true, nil
}

// PromoteTicket is an operator's "Öne al" (docs/event-capacity.md): the
// waitlisted Ticket is active, past the capacity if it must
// (Ticket.OverCapacity), and its mail goes. Until the Event ends, never on
// a cancelled Event; an active Ticket is answered as it is, a cancelled one
// is ErrTicketCancelled (restore it instead).
func (s *service) PromoteTicket(ctx context.Context, p authz.Principal, id uuid.UUID) (Ticket, error) {
	store, err := s.capacityStore()
	if err != nil {
		return Ticket{}, err
	}
	t, ev, err := s.operatedTicket(ctx, p, id)
	if err != nil {
		return Ticket{}, err
	}
	switch t.StatusOf() {
	case StatusActive:
		return s.withEvent(ctx, t), nil
	case StatusCancelled:
		return Ticket{}, ErrTicketCancelled
	}
	if ev.Status == event.StatusCancelled {
		return Ticket{}, ErrEventCancelled
	}
	if ev.EndedAt(time.Now()) {
		return Ticket{}, ErrEventEnded
	}
	record, err := s.beginOperatorAudit(ctx, p, ev, teamaudit.ActionTicketPromote, id)
	if err != nil {
		return Ticket{}, err
	}
	promoted, _, err := store.Promote(ctx, id)
	s.finishTicketAudit(ctx, record, err, 0)
	if err != nil {
		return Ticket{}, err
	}
	return s.withEvent(ctx, promoted), nil
}

// onlyStatuses keeps the Tickets of the statuses given.
func onlyStatuses(tickets []Ticket, statuses []Status) []Ticket {
	out := make([]Ticket, 0, len(tickets))
	for _, t := range tickets {
		for _, status := range statuses {
			if t.StatusOf() == status {
				out = append(out, t)
				break
			}
		}
	}
	return out
}
