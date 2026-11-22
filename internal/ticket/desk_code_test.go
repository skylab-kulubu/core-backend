package ticket_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/doorqr"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
	"github.com/skylab-kulubu/core-backend/internal/testpostgres"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
)

// spoken is how a person might say a code at the desk: small letters, a
// space for the hyphen, and O for 0 and I for 1 where the code has them.
func spoken(code string) string {
	out := strings.ToLower(strings.Replace(code, "-", " ", 1))
	out = strings.ReplaceAll(out, "0", "o")
	return strings.ReplaceAll(out, "1", "i")
}

// The desk finds a Ticket by its code before any name or e-mail, as typed
// or said; the door attendee search finds it alone.
func TestDeskFindsATicketByItsCode(t *testing.T) {
	t.Parallel()
	f := newDoorFixture(t, doorqr.ModeQR)
	ctx := context.Background()
	listed, err := f.svc.ListByEvent(ctx, yk, f.ev.ID)
	if err != nil {
		t.Fatal(err)
	}
	var grace ticket.Ticket
	for _, tk := range listed {
		if tk.GuestEmail == "grace@example.com" {
			grace = tk
		}
	}
	attendees, err := f.svc.SearchDoorAttendees(ctx, f.staff, f.ev.ID, spoken(grace.Code))
	if err != nil || len(attendees) != 1 || attendees[0].Email != "grace@example.com" || attendees[0].Name != "Grace Hopper" {
		t.Fatalf("door attendees by code: %+v %v", attendees, err)
	}
	got, err := f.svc.ResolveAndCheckIn(ctx, f.staff, f.sess.ID, ticket.DoorCheckInTarget{Query: spoken(grace.Code)})
	if err != nil || got.TicketID != grace.ID || got.PersonName != "Grace Hopper" {
		t.Fatalf("resolve by code: %+v %v", got, err)
	}
	if _, err := f.svc.ResolveAndCheckIn(ctx, f.staff, f.sess.ID, ticket.DoorCheckInTarget{Query: grace.Code}); !errors.Is(err, ticket.ErrConflict) {
		t.Fatalf("again: %v", err)
	}
	// A code no Ticket of the Event has falls through to the name search.
	if _, err := f.svc.ResolveAndCheckIn(ctx, f.staff, f.sess.ID, ticket.DoorCheckInTarget{Query: "ZZZZ-ZZZZ"}); !errors.Is(err, ticket.ErrNotFound) {
		t.Fatalf("unknown code: %v", err)
	}
}

// The same on the database, where an erased guest's Ticket is not found by
// its code.
func TestDeskFindsATicketByItsCodeInTheDatabase(t *testing.T) {
	pool := testpostgres.Start(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	events := event.NewPostgresStore(pool)
	staff := authz.Principal{ID: "staff", Groups: []string{"/UYELER/YK"}}
	ev, err := events.Create(ctx, event.Event{Name: "Hack", Location: "YTÜ", OwnerTeam: "WEBLAB"})
	if err != nil {
		t.Fatal(err)
	}
	day := seedDay(t, events, ev.ID, "Gün 1")
	sess := seedSession(t, events, day.ID, "Açılış")
	svc := ticket.NewService(ticket.NewPostgresStore(pool), events, authz.NewAuthorizer(authz.DefaultPolicy()))
	ada, err := svc.ApplyGuest(ctx, weblabLead, ev.ID, ticket.GuestInfo{FirstName: "Ada", LastName: "Lovelace", Email: "ada@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	grace, err := svc.ApplyGuest(ctx, weblabLead, ev.ID, ticket.GuestInfo{FirstName: "Grace", LastName: "Hopper", Email: "grace@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	got, err := svc.ResolveAndCheckIn(ctx, staff, sess.ID, ticket.DoorCheckInTarget{Query: spoken(ada.Ticket.Code)})
	if err != nil || got.TicketID != ada.Ticket.ID || got.PersonName != "Ada Lovelace" {
		t.Fatalf("resolve by code: %+v %v", got, err)
	}
	attendees, err := svc.SearchDoorAttendees(ctx, staff, ev.ID, grace.Ticket.Code)
	if err != nil || len(attendees) != 1 || attendees[0].Email != "grace@example.com" {
		t.Fatalf("door attendees by code: %+v %v", attendees, err)
	}
	// Grace is erased: her Ticket stays, her code finds nothing.
	if _, err := pool.Exec(ctx, `UPDATE tickets SET guest_first_name = '', guest_last_name = '', guest_email = '', access_key = NULL, qr_version = qr_version + 1 WHERE id = $1`, grace.Ticket.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ResolveAndCheckIn(ctx, staff, sess.ID, ticket.DoorCheckInTarget{Query: grace.Ticket.Code}); !errors.Is(err, ticket.ErrNotFound) {
		t.Fatalf("erased guest's code: %v", err)
	}
}
