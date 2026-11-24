package ticket_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// An answer pulled from Forms' list again (a backfill, the daily catch-up)
// takes the forms report's rule: a Ticket an operator cancelled is found as
// one already there and stays cancelled, for a member and a guest alike,
// and nothing is written. Only Forms' own decline is reopened.
func TestAPulledAnswerKeepsACancelledTicketCancelled(t *testing.T) {
	ctx := context.Background()
	events := event.NewMemoryStore()
	users := user.NewMemoryStore()
	tickets := ticket.NewMemoryStore()
	svc := ticket.NewService(tickets, events, authz.NewAuthorizer(authz.DefaultPolicy()), users)
	formID := uuid.New()
	ev := seedFormEvent(t, events, formAddress(formID))
	person := uuid.New()
	seedUser(t, users, person)

	member := ticket.FormResponse{FormID: formID, ResponseID: uuid.New(), Status: ticket.FormResponseAccepted, UserID: &person}
	guest := acceptedGuest(formID, "Ada", "Lovelace", "guest@example.com")
	for _, r := range []ticket.FormResponse{member, guest} {
		if _, err := svc.RecordFormResponse(ctx, formsService, r); err != nil {
			t.Fatal(err)
		}
	}
	for _, tk := range ticketsOf(t, svc, ev) {
		if _, _, err := tickets.Cancel(ctx, tk.ID, ticket.CancelledByOperator, ""); err != nil {
			t.Fatal(err)
		}
	}
	for _, r := range []ticket.FormResponse{member, guest} {
		got, err := svc.RecordFormResponse(ctx, formsService, r)
		if err != nil || got.TicketsWritten != 0 {
			t.Fatalf("pulled: %+v %v", got, err)
		}
	}
	listed, err := svc.ListByEventStatus(ctx, yk, ev.ID, []ticket.Status{ticket.StatusActive, ticket.StatusWaitlisted, ticket.StatusCancelled})
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 2 {
		t.Fatalf("tickets %d", len(listed))
	}
	for _, tk := range listed {
		if tk.StatusOf() != ticket.StatusCancelled {
			t.Fatalf("a pulled answer reopened %+v", tk)
		}
	}
}
