package ticket_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
)

// On the database the holder's cancellation (kontenjan-iptal-hatirlatma
// 03, 04) queues "Biletin iptal edildi" in the transaction that cancels:
// the row of a mail still waiting becomes the cancellation's; the place
// goes to the waiting list there too. With the status mails off nothing is
// queued and a waiting mail is dropped; an operator who does not tell
// queues nothing.
func TestCancellationMailOnPostgres(t *testing.T) {
	e := newCapacityPG(t)
	ctx := context.Background()
	ev := e.event(t, 1, true)
	first, err := e.store.CreateAdmitted(ctx, guestTicket(ev.ID, 0), ticket.Admission{})
	if err != nil {
		t.Fatal(err)
	}
	waiting, err := e.store.CreateAdmitted(ctx, guestTicket(ev.ID, 1), ticket.Admission{Waitlist: true})
	if err != nil {
		t.Fatal(err)
	}
	if kind := e.mailKind(t, first.ID); kind != "created" {
		t.Fatalf("first mail %q", kind)
	}
	got, err := ticket.HolderCancel(ctx, e.store, e.events, first, first.CreatedAt)
	if err != nil || got.StatusOf() != ticket.StatusCancelled || got.CancelledBy != ticket.CancelledByHolder {
		t.Fatalf("%+v %v", got, err)
	}
	if kind := e.mailKind(t, first.ID); kind != "cancelled" {
		t.Fatalf("cancellation mail %q", kind)
	}
	if kind := e.mailKind(t, waiting.ID); kind != "promoted" {
		t.Fatalf("the next in line %q", kind)
	}
	if c := e.counts(t, ev.ID); c[ticket.StatusActive] != 1 || c[ticket.StatusCancelled] != 1 {
		t.Fatalf("counts %v", c)
	}
	// Again: answered as it is, the row stays one.
	if _, err := ticket.HolderCancel(ctx, e.store, e.events, got, got.CreatedAt); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := e.pool.QueryRow(ctx, `SELECT count(*) FROM ticket_notifications WHERE ticket_id = $1`, first.ID).Scan(&n); err != nil || n != 1 {
		t.Fatalf("%d rows %v", n, err)
	}

	// An operator who does not tell drops the waiting mail.
	third, err := e.store.CreateAdmitted(ctx, guestTicket(ev.ID, 2), ticket.Admission{Over: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.store.CancelTelling(ctx, third.ID, ticket.CancelledByOperator, "", false); err != nil {
		t.Fatal(err)
	}
	if kind := e.mailKind(t, third.ID); kind != "" {
		t.Fatalf("silent operator queued %q", kind)
	}

	// The status mails off: the holder's cancellation queues nothing.
	e.store.NotifyStatus(false)
	fourth, err := e.store.CreateAdmitted(ctx, guestTicket(ev.ID, 3), ticket.Admission{Over: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.store.CancelTelling(ctx, fourth.ID, ticket.CancelledByHolder, "", true); err != nil {
		t.Fatal(err)
	}
	if kind := e.mailKind(t, fourth.ID); kind != "" {
		t.Fatalf("mails off queued %q", kind)
	}
}

// tickets.form_response_id: an accepted answer's Ticket keeps it, a Ticket
// written another way has none, and a reopening another way clears it.
func TestFormResponseIDOnPostgres(t *testing.T) {
	e := newCapacityPG(t)
	ctx := context.Background()
	ev := e.event(t, 0, true)
	answer := uuid.New()
	fromForm, err := e.store.CreateAdmitted(ctx, guestTicket(ev.ID, 0), ticket.Admission{Waitlist: true, FormResponse: answer})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.CreateAdmitted(ctx, guestTicket(ev.ID, 1), ticket.Admission{}); err != nil {
		t.Fatal(err)
	}
	found, err := e.store.FormResponseTickets(ctx, answer)
	if err != nil || len(found) != 1 || found[0].ID != fromForm.ID {
		t.Fatalf("found %+v %v", found, err)
	}
	if none, err := e.store.FormResponseTickets(ctx, uuid.Nil); err != nil || len(none) != 0 {
		t.Fatalf("nil answer %+v %v", none, err)
	}
	if _, _, err := e.store.Cancel(ctx, fromForm.ID, ticket.CancelledByForm, ""); err != nil {
		t.Fatal(err)
	}
	if kind := e.mailKind(t, fromForm.ID); kind != "" {
		t.Fatalf("the form's cancellation queued %q", kind)
	}
	if _, _, err := e.store.Reopen(ctx, fromForm.ID); err != nil {
		t.Fatal(err)
	}
	if found, err := e.store.FormResponseTickets(ctx, answer); err != nil || len(found) != 0 {
		t.Fatalf("after the panel's restore %+v %v", found, err)
	}
}
