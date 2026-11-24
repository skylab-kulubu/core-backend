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

// accepted → declined → accepted: the decline cancels the Ticket the answer
// wrote (by the form, no mail), the next acceptance reopens it.
func TestFormDeclineCancelsTheAnswersTicketAndAcceptanceReopensIt(t *testing.T) {
	t.Parallel()
	e := newDeclineEnv(t)
	r := acceptedGuest(e.formID, "Ada", "Lovelace", "ada@example.com")
	if got := e.report(t, r); got.Outcome != ticket.FormResponseRecorded || got.TicketsWritten != 1 {
		t.Fatalf("accepted %+v", got)
	}
	written := storedGuest(t, e.svc, e.ev, "ada@example.com")

	got := e.report(t, declined(r))
	if got.Outcome != ticket.FormResponseCancelled || got.TicketsCancelled != 1 || got.ListingEvents != 1 {
		t.Fatalf("declined %+v", got)
	}
	stored, _ := e.store.Get(context.Background(), written.ID)
	if stored.StatusOf() != ticket.StatusCancelled || stored.CancelledBy != ticket.CancelledByForm || e.store.CancelMails[written.ID] != 0 {
		t.Fatalf("cancelled %+v (mails %v)", stored, e.store.CancelMails)
	}
	// Declined again: nothing more to cancel.
	if got := e.report(t, declined(r)); got.Outcome != ticket.FormResponseNotAccepted || got.TicketsCancelled != 0 {
		t.Fatalf("declined again %+v", got)
	}

	if got := e.report(t, r); got.Outcome != ticket.FormResponseRecorded || got.TicketsWritten != 1 {
		t.Fatalf("accepted again %+v", got)
	}
	reopened, _ := e.store.Get(context.Background(), written.ID)
	if reopened.StatusOf() != ticket.StatusActive || reopened.QRVersion != stored.QRVersion+1 {
		t.Fatalf("reopened %+v", reopened)
	}
	// The reopening answer is the Ticket's again: its decline cancels it.
	if got := e.report(t, declined(r)); got.TicketsCancelled != 1 {
		t.Fatalf("declined after reopening %+v", got)
	}
}

// A decline that comes first writes and cancels nothing.
func TestFormDeclineFirstWritesNothing(t *testing.T) {
	t.Parallel()
	e := newDeclineEnv(t)
	got := e.report(t, declined(acceptedGuest(e.formID, "Ada", "Lovelace", "ada@example.com")))
	if got.Outcome != ticket.FormResponseNotAccepted || got.TicketsCancelled != 0 {
		t.Fatalf("%+v", got)
	}
	if listed := ticketsOf(t, e.svc, e.ev); len(listed) != 0 {
		t.Fatalf("tickets %+v", listed)
	}
}

// A Ticket another way wrote is not the answer's: an acceptance finds it
// already there and does not claim it, so the decline leaves it. Another
// answer's decline does not reach it either.
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
	if got := e.report(t, declined(r)); got.TicketsCancelled != 0 {
		t.Fatalf("declined %+v", got)
	}
	other := acceptedGuest(e.formID, "Grace", "Hopper", "grace@example.com")
	e.report(t, other)
	if got := e.report(t, declined(acceptedGuest(e.formID, "Grace", "Hopper", "grace@example.com"))); got.TicketsCancelled != 0 {
		t.Fatalf("another answer's decline %+v", got)
	}
	for _, email := range []string{"ada@example.com", "grace@example.com"} {
		if tk := storedGuest(t, e.svc, e.ev, email); tk.StatusOf() != ticket.StatusActive {
			t.Fatalf("%s %+v", email, tk)
		}
	}
	stored, _ := e.store.Get(ctx, applied.Ticket.ID)
	if stored.StatusOf() != ticket.StatusActive {
		t.Fatalf("panel's Ticket %+v", stored)
	}
}

// A Ticket with a check-in is not cancelled by a decline; nor is one the
// panel restored after a decline (it keeps no answer since).
func TestFormDeclineLeavesACheckedInOrRestoredTicket(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := newDeclineEnv(t)
	r := acceptedGuest(e.formID, "Ada", "Lovelace", "ada@example.com")
	e.report(t, r)
	ada := storedGuest(t, e.svc, e.ev, "ada@example.com")
	if _, err := e.svc.CheckIn(ctx, weblabLead, ada.ID, e.sess.ID); err != nil {
		t.Fatal(err)
	}
	if got := e.report(t, declined(r)); got.TicketsCancelled != 0 || got.Outcome != ticket.FormResponseNotAccepted {
		t.Fatalf("checked in %+v", got)
	}

	g := acceptedGuest(e.formID, "Grace", "Hopper", "grace@example.com")
	e.report(t, g)
	e.report(t, declined(g))
	grace, _ := e.store.GetByGuestEvent(ctx, "grace@example.com", e.ev.ID)
	if _, err := e.svc.RestoreTicket(ctx, weblabLead, grace.ID); err != nil {
		t.Fatal(err)
	}
	if got := e.report(t, declined(g)); got.TicketsCancelled != 0 {
		t.Fatalf("restored %+v", got)
	}
}

// A Ticket written before the answer was kept (no form_response_id) is not
// declined.
func TestFormDeclineLeavesAnOldTicket(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := newDeclineEnv(t)
	old, err := e.store.Create(ctx, ticket.Ticket{EventID: e.ev.ID, TicketType: ticket.Guest, GuestFirstName: "Ada", GuestLastName: "Lovelace", GuestEmail: "ada@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if got := e.report(t, declined(acceptedGuest(e.formID, "Ada", "Lovelace", "ada@example.com"))); got.TicketsCancelled != 0 {
		t.Fatalf("%+v", got)
	}
	stored, _ := e.store.Get(ctx, old.ID)
	if stored.StatusOf() != ticket.StatusActive {
		t.Fatalf("%+v", stored)
	}
}
