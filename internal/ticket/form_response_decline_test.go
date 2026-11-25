package ticket_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
)

// declineEnv is an Event of WEBLAB that lists a form, with a Day and a
// Session for check-ins, on the memory stores.
type declineEnv struct {
	events event.Store
	store  *ticket.MemoryStore
	svc    ticket.Service
	ev     event.Event
	sess   event.Session
	formID uuid.UUID
}

func newDeclineEnv(t *testing.T) declineEnv {
	t.Helper()
	e := declineEnv{events: event.NewMemoryStore(), store: ticket.NewMemoryStore(), formID: uuid.New()}
	e.svc = ticket.NewService(e.store, e.events, authz.NewAuthorizer(authz.DefaultPolicy()))
	e.ev = seedFormEvent(t, e.events, formAddress(e.formID))
	day := seedDay(t, e.events, e.ev.ID, "Gün 1")
	e.sess = seedSession(t, e.events, day.ID, "Açılış")
	return e
}

func (e declineEnv) report(t *testing.T, r ticket.FormResponse) ticket.FormResponseResult {
	t.Helper()
	got, err := e.svc.RecordFormResponse(context.Background(), formsService, r)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func declined(r ticket.FormResponse) ticket.FormResponse {
	r.Status = ticket.FormResponseDeclined
	return r
}

// accepted → declined → accepted (K6, Yusuf 2026-11-25): the decline marks
// the Ticket the answer wrote, cancels nothing and mails nobody; the next
// acceptance clears the mark.
func TestFormDeclineMarksTheAnswersTicketAndAcceptanceClearsIt(t *testing.T) {
	t.Parallel()
	e := newDeclineEnv(t)
	r := acceptedGuest(e.formID, "Ada", "Lovelace", "ada@example.com")
	if got := e.report(t, r); got.Outcome != ticket.FormResponseRecorded || got.TicketsWritten != 1 {
		t.Fatalf("accepted %+v", got)
	}
	written := storedGuest(t, e.svc, e.ev, "ada@example.com")

	got := e.report(t, declined(r))
	if got.Outcome != ticket.FormResponseFormsRejected || got.TicketsMarked != 1 || got.TicketsCancelled != 0 || got.ListingEvents != 1 {
		t.Fatalf("declined %+v", got)
	}
	stored, _ := e.store.Get(context.Background(), written.ID)
	if stored.StatusOf() != ticket.StatusActive || stored.FormsRejectedAt == nil || e.store.CancelMails[written.ID] != 0 {
		t.Fatalf("marked %+v (mails %v)", stored, e.store.CancelMails)
	}
	// The panel reads the mark on the Event's Tickets.
	if listed := storedGuest(t, e.svc, e.ev, "ada@example.com"); listed.FormsRejectedAt == nil {
		t.Fatalf("listed %+v", listed)
	}
	// Declined again: marked already.
	if got := e.report(t, declined(r)); got.Outcome != ticket.FormResponseNotAccepted || got.TicketsMarked != 0 {
		t.Fatalf("declined again %+v", got)
	}

	if got := e.report(t, r); got.Outcome != ticket.FormResponseRecorded || got.TicketsWritten != 0 {
		t.Fatalf("accepted again %+v", got)
	}
	cleared, _ := e.store.Get(context.Background(), written.ID)
	if cleared.StatusOf() != ticket.StatusActive || cleared.FormsRejectedAt != nil || cleared.QRVersion != stored.QRVersion {
		t.Fatalf("cleared %+v", cleared)
	}
}

// Forms asks for a correction (Returned, sent as declined) and the person
// sends a new answer, which is accepted: the Ticket follows the new answer.
// The old answer's decline, in either order, leaves it unmarked.
func TestACorrectedAnswerKeepsTheTicket(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for _, declineFirst := range []bool{true, false} {
		e := newDeclineEnv(t)
		first := acceptedGuest(e.formID, "Ada", "Lovelace", "ada@example.com")
		second := acceptedGuest(e.formID, "Ada", "Lovelace", "ada@example.com")
		e.report(t, first)
		if declineFirst {
			e.report(t, declined(first))
			e.report(t, second)
		} else {
			e.report(t, second)
			if got := e.report(t, declined(first)); got.TicketsMarked != 0 {
				t.Fatalf("the old answer's late decline %+v", got)
			}
		}
		tk := storedGuest(t, e.svc, e.ev, "ada@example.com")
		stored, _ := e.store.Get(ctx, tk.ID)
		if stored.StatusOf() != ticket.StatusActive || stored.FormsRejectedAt != nil {
			t.Fatalf("decline first %t: %+v", declineFirst, stored)
		}
		// The new answer's decline reaches it.
		if got := e.report(t, declined(second)); got.TicketsMarked != 1 {
			t.Fatalf("decline first %t, the new answer's decline %+v", declineFirst, got)
		}
	}
}

// A decline that comes first writes and marks nothing.
func TestFormDeclineFirstWritesNothing(t *testing.T) {
	t.Parallel()
	e := newDeclineEnv(t)
	got := e.report(t, declined(acceptedGuest(e.formID, "Ada", "Lovelace", "ada@example.com")))
	if got.Outcome != ticket.FormResponseNotAccepted || got.TicketsMarked != 0 {
		t.Fatalf("%+v", got)
	}
	if listed := ticketsOf(t, e.svc, e.ev); len(listed) != 0 {
		t.Fatalf("tickets %+v", listed)
	}
}

// A Ticket another way wrote is not the answer's: an acceptance finds it
// already there and does not claim it, so the decline leaves it unmarked.
// Another answer's decline does not reach it either.
func TestFormDeclineLeavesATicketAnotherWayWrote(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := newDeclineEnv(t)
	applied, err := e.svc.ApplyGuest(ctx, weblabLead, e.ev.ID, ticket.GuestInfo{FirstName: "Ada", LastName: "Lovelace", Email: "ada@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	r := acceptedGuest(e.formID, "Ada", "Lovelace", "ada@example.com")
	if got := e.report(t, r); got.TicketsWritten != 0 {
		t.Fatalf("already there %+v", got)
	}
	if got := e.report(t, declined(r)); got.TicketsMarked != 0 {
		t.Fatalf("declined %+v", got)
	}
	other := acceptedGuest(e.formID, "Grace", "Hopper", "grace@example.com")
	e.report(t, other)
	if got := e.report(t, declined(acceptedGuest(e.formID, "Grace", "Hopper", "grace@example.com"))); got.TicketsMarked != 0 {
		t.Fatalf("another answer's decline %+v", got)
	}
	for _, email := range []string{"ada@example.com", "grace@example.com"} {
		if tk := storedGuest(t, e.svc, e.ev, email); tk.StatusOf() != ticket.StatusActive || tk.FormsRejectedAt != nil {
			t.Fatalf("%s %+v", email, tk)
		}
	}
	stored, _ := e.store.Get(ctx, applied.Ticket.ID)
	if stored.StatusOf() != ticket.StatusActive {
		t.Fatalf("panel's Ticket %+v", stored)
	}
}

// A Ticket with a check-in is marked, never cancelled; one the holder or an
// operator cancelled is not marked.
func TestFormDeclineMarksACheckedInTicketAndLeavesACancelledOne(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := newDeclineEnv(t)
	r := acceptedGuest(e.formID, "Ada", "Lovelace", "ada@example.com")
	e.report(t, r)
	ada := storedGuest(t, e.svc, e.ev, "ada@example.com")
	if _, err := e.svc.CheckIn(ctx, weblabLead, ada.ID, e.sess.ID); err != nil {
		t.Fatal(err)
	}
	if got := e.report(t, declined(r)); got.TicketsMarked != 1 || got.Outcome != ticket.FormResponseFormsRejected {
		t.Fatalf("checked in %+v", got)
	}
	if stored, _ := e.store.Get(ctx, ada.ID); stored.StatusOf() != ticket.StatusActive {
		t.Fatalf("checked in %+v", stored)
	}

	g := acceptedGuest(e.formID, "Grace", "Hopper", "grace@example.com")
	e.report(t, g)
	grace, _ := e.store.GetByGuestEvent(ctx, "grace@example.com", e.ev.ID)
	if _, _, err := e.store.Cancel(ctx, grace.ID, ticket.CancelledByOperator, ""); err != nil {
		t.Fatal(err)
	}
	if got := e.report(t, declined(g)); got.TicketsMarked != 0 {
		t.Fatalf("cancelled %+v", got)
	}
	if stored, _ := e.store.Get(ctx, grace.ID); stored.FormsRejectedAt != nil {
		t.Fatalf("cancelled %+v", stored)
	}
}

// A Ticket written before the answer was kept (no form_response_id) is not
// marked.
func TestFormDeclineLeavesAnOldTicket(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := newDeclineEnv(t)
	old, err := e.store.Create(ctx, ticket.Ticket{EventID: e.ev.ID, TicketType: ticket.Guest, GuestFirstName: "Ada", GuestLastName: "Lovelace", GuestEmail: "ada@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if got := e.report(t, declined(acceptedGuest(e.formID, "Ada", "Lovelace", "ada@example.com"))); got.TicketsMarked != 0 {
		t.Fatalf("%+v", got)
	}
	stored, _ := e.store.Get(ctx, old.ID)
	if stored.StatusOf() != ticket.StatusActive || stored.FormsRejectedAt != nil {
		t.Fatalf("%+v", stored)
	}
}
