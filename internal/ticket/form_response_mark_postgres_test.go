package ticket_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
)

// K6 on PostgreSQL: a decline marks the answer's Ticket (no cancellation,
// no mail), the Event's list carries the mark, and the corrected answer's
// acceptance moves the Ticket to it and clears the mark.
func TestFormDeclineMarksOnPostgres(t *testing.T) {
	e := newCapacityPG(t)
	ctx := context.Background()
	ev := e.event(t, 0, true)
	formID := uuid.New()
	ev.FormURL = formAddress(formID)
	if _, err := e.events.Update(ctx, ev); err != nil {
		t.Fatal(err)
	}
	svc := ticket.NewService(e.store, e.events, authz.NewAuthorizer(authz.DefaultPolicy()))
	report := func(r ticket.FormResponse) ticket.FormResponseResult {
		t.Helper()
		got, err := svc.RecordFormResponse(ctx, formsService, r)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	first := acceptedGuest(formID, "Ada", "Lovelace", "ada@example.com")
	report(first)
	written, err := e.store.GetByGuestEvent(ctx, "ada@example.com", ev.ID)
	if err != nil {
		t.Fatal(err)
	}
	mail := e.mailKind(t, written.ID)
	first.Status = ticket.FormResponseDeclined
	if got := report(first); got.Outcome != ticket.FormResponseFormsRejected || got.TicketsMarked != 1 {
		t.Fatalf("decline %+v", got)
	}
	listed, err := svc.ListByEventStatus(ctx, weblabLead, ev.ID, nil)
	if err != nil || len(listed) != 1 || listed[0].FormsRejectedAt == nil || listed[0].StatusOf() != ticket.StatusActive || e.mailKind(t, written.ID) != mail {
		t.Fatalf("listed %+v %v (mail %q)", listed, err, e.mailKind(t, written.ID))
	}
	if got := report(first); got.TicketsMarked != 0 {
		t.Fatalf("declined again %+v", got)
	}
	second := acceptedGuest(formID, "Ada", "Lovelace", "ada@example.com")
	report(second)
	got, err := e.store.Get(ctx, written.ID)
	if err != nil || got.FormsRejectedAt != nil || got.StatusOf() != ticket.StatusActive {
		t.Fatalf("after the corrected answer %+v %v", got, err)
	}
	if tickets, err := e.store.FormResponseTickets(ctx, second.ResponseID); err != nil || len(tickets) != 1 {
		t.Fatalf("the corrected answer's Tickets %+v %v", tickets, err)
	}
}
