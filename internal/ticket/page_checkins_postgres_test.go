package ticket_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
)

// The ticket page's poll reads, in one query, the Ticket its key opens:
// held or not, its Event's status and dates, and its own check-ins with the
// Session's title, oldest first; nothing of another Ticket's. An archived
// Event's key and an unknown key are not found.
func TestPageCheckInsOnPostgres(t *testing.T) {
	f := startFormResponsePostgres(t)
	ctx := context.Background()
	store := ticket.NewPostgresStore(f.pool)
	svc := f.service(store)
	start := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	end := start.Add(4 * time.Hour)
	ev, err := f.events.Create(ctx, event.Event{Name: "Hack", Location: "YTÜ", OwnerTeam: "WEBLAB", StartDate: &start, EndDate: &end})
	if err != nil {
		t.Fatal(err)
	}
	day := seedDay(t, f.events, ev.ID, "Day 1")
	opening := seedSession(t, f.events, day.ID, "Açılış")
	closing := seedSession(t, f.events, day.ID, "Kapanış")
	guest, err := store.Create(ctx, ticket.Ticket{EventID: ev.ID, TicketType: ticket.Guest, GuestFirstName: "Ada", GuestLastName: "Lovelace", GuestEmail: "ada@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	other, err := store.Create(ctx, ticket.Ticket{EventID: ev.ID, TicketType: ticket.Guest, GuestFirstName: "Grace", GuestLastName: "Hopper", GuestEmail: "grace@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	key, _, _ := f.storedKeys(t, guest.ID)

	got, err := store.PageCheckIns(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Held || got.Event.ID != ev.ID || got.Event.Status != ev.Status || len(got.CheckIns) != 0 ||
		got.Event.StartDate == nil || !got.Event.StartDate.Equal(start) || got.Event.EndDate == nil || !got.Event.EndDate.Equal(end) {
		t.Fatalf("before: %+v", got)
	}

	for _, id := range []struct{ ticket, session uuid.UUID }{{guest.ID, opening.ID}, {other.ID, opening.ID}, {guest.ID, closing.ID}} {
		if _, err := svc.CheckIn(ctx, weblabLead, id.ticket, id.session); err != nil {
			t.Fatal(err)
		}
	}
	got, err = store.PageCheckIns(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.CheckIns) != 2 || got.CheckIns[0].Session != "Açılış" || got.CheckIns[1].Session != "Kapanış" ||
		got.CheckIns[0].At.IsZero() || got.CheckIns[1].At.Before(got.CheckIns[0].At) {
		t.Fatalf("after: %+v", got.CheckIns)
	}

	if _, err := store.PageCheckIns(ctx, "ABCDEFGHIJKLMNOPQRSTUVWXYZ"); !errors.Is(err, ticket.ErrNotFound) {
		t.Fatalf("unknown key: %v", err)
	}
	if err := f.events.Archive(ctx, ev.ID, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PageCheckIns(ctx, key); !errors.Is(err, ticket.ErrNotFound) {
		t.Fatalf("archived: %v", err)
	}
}
