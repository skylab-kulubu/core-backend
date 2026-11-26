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
// sends a corrected answer to the same form, which is accepted: the Ticket
// follows the corrected answer and loses its mark; the old answer's decline
// again reaches nothing; the corrected answer's decline marks it. Pending
// again works as a decline does here.
func TestACorrectedAnswerTakesTheTicketOver(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for _, how := range []ticket.FormResponseStatus{ticket.FormResponseDeclined, ticket.FormResponsePending} {
		e := newDeclineEnv(t)
		first := acceptedGuest(e.formID, "Ada", "Lovelace", "ada@example.com")
		second := acceptedGuest(e.formID, "Ada", "Lovelace", "ada@example.com")
		e.report(t, first)
		again := first
		again.Status = how
		e.report(t, again)
		if got := e.report(t, second); got.TicketsWritten != 0 {
			t.Fatalf("%s: corrected %+v", how, got)
		}
		tk := storedGuest(t, e.svc, e.ev, "ada@example.com")
		stored, _ := e.store.Get(ctx, tk.ID)
		if stored.StatusOf() != ticket.StatusActive || stored.FormsRejectedAt != nil {
			t.Fatalf("%s: %+v", how, stored)
		}
		if got := e.report(t, declined(first)); got.TicketsMarked != 0 {
			t.Fatalf("%s: the old answer's decline %+v", how, got)
		}
		if got := e.report(t, declined(second)); got.TicketsMarked != 1 {
			t.Fatalf("%s: the corrected answer's decline %+v", how, got)
		}
	}
}

// While the holder's answer stands, another accepted answer to the same
// form with the holder's e-mail (anyone may type it into a form without
// sign-in) takes nothing over: its decline marks nothing, the holder's
// decline still marks the Ticket.
func TestSomeoneElsesAnswerWithTheHoldersEmailTakesNothing(t *testing.T) {
	t.Parallel()
	e := newDeclineEnv(t)
	holder := acceptedGuest(e.formID, "Ada", "Lovelace", "ada@example.com")
	other := acceptedGuest(e.formID, "Eve", "Forged", "ada@example.com")
	e.report(t, holder)
	if got := e.report(t, other); got.TicketsWritten != 0 {
		t.Fatalf("other %+v", got)
	}
	if got := e.report(t, declined(other)); got.TicketsMarked != 0 {
		t.Fatalf("the other answer's decline %+v", got)
	}
	if got := e.report(t, declined(holder)); got.TicketsMarked != 1 {
		t.Fatalf("the holder's decline %+v", got)
	}
	if tk := storedGuest(t, e.svc, e.ev, "ada@example.com"); tk.GuestFirstName != "Ada" {
		t.Fatalf("renamed %+v", tk)
	}
}

// An extra form's accepted answer never takes over the application's
// Ticket: the application's decline still marks it, the extra form's does
// not.
func TestAnExtraFormsAnswerTakesNothingOver(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	events := event.NewMemoryStore()
	store := ticket.NewMemoryStore()
	svc := ticket.NewService(store, events, authz.NewAuthorizer(authz.DefaultPolicy()))
	application, extra := uuid.New(), uuid.New()
	ev := seedFormEvent(t, events, formAddress(application), event.EventFormLink{URL: formAddress(extra)})
	e := declineEnv{events: events, store: store, svc: svc, ev: ev, formID: application}
	main := acceptedGuest(application, "Ada", "Lovelace", "ada@example.com")
	more := acceptedGuest(extra, "Ada", "Lovelace", "ada@example.com")
	e.report(t, main)
	if got := e.report(t, more); got.TicketsWritten != 0 {
		t.Fatalf("extra %+v", got)
	}
	if got := e.report(t, declined(more)); got.TicketsMarked != 0 {
		t.Fatalf("the extra form's decline %+v", got)
	}
	if got := e.report(t, declined(main)); got.TicketsMarked != 1 {
		t.Fatalf("the application's decline %+v", got)
	}
	// Even with the application's answer declined, the extra form's next
	// acceptance does not take the Ticket over.
	e.report(t, acceptedGuest(extra, "Ada", "Lovelace", "ada@example.com"))
	tk := storedGuest(t, svc, ev, "ada@example.com")
	if stored, _ := store.Get(ctx, tk.ID); stored.FormsRejectedAt == nil {
		t.Fatalf("an extra form's answer cleared the mark: %+v", stored)
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

// A signed-in holder's Ticket is kept only for that holder's answers: the
// store refuses another person's, and a guest's.
func TestKeepFormResponseNeedsTheHolder(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := newDeclineEnv(t)
	owner, answer := uuid.New(), uuid.New()
	written, err := e.store.CreateAdmitted(ctx, ticket.Ticket{EventID: e.ev.ID, TicketType: ticket.Registered, OwnerID: &owner}, ticket.Admission{FormResponse: answer})
	if err != nil {
		t.Fatal(err)
	}
	stranger := uuid.New()
	for name, who := range map[string]*uuid.UUID{"another person": &stranger, "a guest": nil} {
		if kept, err := e.store.KeepFormResponse(ctx, written.ID, e.formID, answer, who); err != nil || kept {
			t.Fatalf("%s: %t %v", name, kept, err)
		}
	}
	if kept, err := e.store.KeepFormResponse(ctx, written.ID, e.formID, answer, &owner); err != nil || !kept {
		t.Fatalf("the holder: %t %v", kept, err)
	}
}

// A Ticket whose answer was kept before its form was (migration
// 20261127110000) takes the form from its decline: the corrected answer to
// that form then takes it over.
func TestADeclineFillsAnOldTicketsForm(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := newDeclineEnv(t)
	first := acceptedGuest(e.formID, "Ada", "Lovelace", "ada@example.com")
	old, err := e.store.CreateAdmitted(ctx, ticket.Ticket{EventID: e.ev.ID, TicketType: ticket.Guest, GuestFirstName: "Ada", GuestLastName: "Lovelace", GuestEmail: "ada@example.com"}, ticket.Admission{FormResponse: first.ResponseID})
	if err != nil {
		t.Fatal(err)
	}
	if got := e.report(t, declined(first)); got.TicketsMarked != 1 {
		t.Fatalf("decline %+v", got)
	}
	second := acceptedGuest(e.formID, "Ada", "Lovelace", "ada@example.com")
	e.report(t, second)
	stored, _ := e.store.Get(ctx, old.ID)
	if stored.FormsRejectedAt != nil {
		t.Fatalf("not taken over %+v", stored)
	}
	if tickets, _ := e.store.FormResponseTickets(ctx, second.ResponseID); len(tickets) != 1 {
		t.Fatalf("the corrected answer's Tickets %+v", tickets)
	}
}
