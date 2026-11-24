package ticket_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
)

// The forms report (HTTP or a core.forms event) reopens only what Forms'
// own decline cancelled. The holder's own cancellation and an operator's
// stand: an accepted report leaves the Ticket cancelled and queues no
// mail. A decline, then an acceptance, reopens it with "Biletin hazır".
func TestFormReportReopensOnlyFormsOwnCancellationOnPostgres(t *testing.T) {
	e := newCapacityPG(t)
	ctx := context.Background()
	formID := uuid.New()
	ev := e.event(t, 0, true)
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
	written := func(email string) ticket.Ticket {
		t.Helper()
		got, err := e.store.GetByGuestEvent(ctx, email, ev.ID)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}

	// The holder cancelled their own Ticket.
	own := acceptedGuest(formID, "Ada", "Lovelace", "own@example.com")
	report(own)
	first := written("own@example.com")
	if _, err := ticket.HolderCancel(ctx, e.store, e.events, first, first.CreatedAt); err != nil {
		t.Fatal(err)
	}
	// An operator cancelled the other.
	byOperator := acceptedGuest(formID, "Grace", "Hopper", "op@example.com")
	report(byOperator)
	second := written("op@example.com")
	if _, _, err := e.store.CancelTelling(ctx, second.ID, ticket.CancelledByOperator, "", false); err != nil {
		t.Fatal(err)
	}
	kinds := map[uuid.UUID]string{first.ID: e.mailKind(t, first.ID), second.ID: e.mailKind(t, second.ID)}
	for _, r := range []ticket.FormResponse{own, byOperator} {
		if got := report(r); got.TicketsWritten != 0 {
			t.Fatalf("an accepted report reopened: %+v", got)
		}
	}
	for id, kind := range kinds {
		got, err := e.store.Get(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if got.StatusOf() != ticket.StatusCancelled || e.mailKind(t, id) != kind {
			t.Fatalf("%s: status %s, mail %q → %q", id, got.StatusOf(), kind, e.mailKind(t, id))
		}
	}

	// Forms' own decline, then an acceptance: reopened, mailed again.
	declinedThenAccepted := acceptedGuest(formID, "Alan", "Turing", "form@example.com")
	report(declinedThenAccepted)
	declinedThenAccepted.Status = ticket.FormResponseDeclined
	if got := report(declinedThenAccepted); got.Outcome != ticket.FormResponseCancelled {
		t.Fatalf("decline %+v", got)
	}
	declinedThenAccepted.Status = ticket.FormResponseAccepted
	if got := report(declinedThenAccepted); got.TicketsWritten != 1 {
		t.Fatalf("acceptance after the decline %+v", got)
	}
	third := written("form@example.com")
	if third.StatusOf() != ticket.StatusActive || e.mailKind(t, third.ID) != "created" {
		t.Fatalf("reopened %s, mail %q", third.StatusOf(), e.mailKind(t, third.ID))
	}
}
