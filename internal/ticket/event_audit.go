package ticket

import (
	"context"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/teamaudit"
)

// EventAuditor opens the Team audit row of an operation another package
// runs on an Event's Tickets (a raffle, docs/raffle.md) on the basis the
// ticket operators' rows take: privileged (ticket:manage or event:manage),
// else a Leader of the Event's team or of one above it, else the Event's
// editor through its team. The ticket service is one; the row is opened
// with the actor the request carries (teamaudit.WithActor), and without
// one it is refused (teamaudit.ErrUnavailable).
type EventAuditor interface {
	BeginEventAudit(ctx context.Context, p authz.Principal, ev event.Event, action teamaudit.Action) (*EventAudit, error)
}

// EventAudit is an open row of BeginEventAudit; a nil one records nothing
// (no audit writer, a test).
type EventAudit struct {
	s      *service
	record *auditRecord
}

// BeginEventAudit opens action's row about ev, no one Ticket.
func (s *service) BeginEventAudit(ctx context.Context, p authz.Principal, ev event.Event, action teamaudit.Action) (*EventAudit, error) {
	record, err := s.beginTicketAuditBasis(ctx, p, ev, action, uuid.Nil, s.privilegedOperator(p), true)
	if err != nil {
		return nil, err
	}
	if record == nil {
		return nil, nil
	}
	return &EventAudit{s: s, record: record}, nil
}

// Finish writes the row's outcome: ok, or failed when err is set; set adds
// what the operation did to the row's after.
func (a *EventAudit) Finish(ctx context.Context, err error, set func(*teamaudit.State)) {
	if a == nil || a.record == nil {
		return
	}
	outcome, code := teamaudit.OutcomeOK, ""
	if err != nil {
		outcome, code = teamaudit.OutcomeFailed, "failed"
	}
	after := a.record.after
	if set != nil {
		set(&after)
	}
	if ferr := a.s.audit.Finish(ctx, a.record.id, outcome, code, &after); ferr != nil {
		a.s.logger.Printf("tickets: audit record %s stays pending", a.record.id)
	}
}
