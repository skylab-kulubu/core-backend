package ticket_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
)

// On PostgreSQL: a guest's Ticket cancelled, then its answer pulled again
// (a backfill, and the daily catch-up overlapping it): the Ticket stays
// cancelled and no mail is queued.
func TestPostgresPulledAnswerQueuesNoMailForACancelledTicket(t *testing.T) {
	e := newCapacityPG(t)
	ctx := context.Background()
	formID := uuid.New()
	ev := e.event(t, 0, true)
	ev.FormURL = formAddress(formID)
	if _, err := e.events.Update(ctx, ev); err != nil {
		t.Fatal(err)
	}
	svc := ticket.NewService(e.store, e.events, authz.NewAuthorizer(authz.DefaultPolicy()))
	answer := acceptedGuest(formID, "Ada", "Lovelace", "ada@example.com")
	if _, err := svc.RecordFormResponse(ctx, formsService, answer); err != nil {
		t.Fatal(err)
	}
	got, err := e.store.GetByGuestEvent(ctx, "ada@example.com", ev.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.store.Cancel(ctx, got.ID, ticket.CancelledByOperator, ""); err != nil {
		t.Fatal(err)
	}
	mails := func() int {
		var n int
		if err := e.pool.QueryRow(ctx, `SELECT count(*) FROM ticket_notifications WHERE ticket_id = $1`, got.ID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	before := mails()
	answer.Pulled = true
	for range 2 {
		if _, err := svc.RecordFormResponse(ctx, formsService, answer); err != nil {
			t.Fatal(err)
		}
	}
	after, err := e.store.Get(ctx, got.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.StatusOf() != ticket.StatusCancelled || mails() != before {
		t.Fatalf("status %s, mails %d → %d", after.StatusOf(), before, mails())
	}
}
