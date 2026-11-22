package ticket_test

import (
	"context"
	"testing"
	"time"

	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
	"github.com/skylab-kulubu/core-backend/internal/testpostgres"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
)

// A guest the panel adds to a draft (K5) is told of the Ticket as a person
// added there is: not while it is a draft (the worker drops the row), and
// once the organizer asks when publishing it (K3).
func TestDraftGuestIsToldWhenThePublicationAsks(t *testing.T) {
	pool := testpostgres.Start(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	az := authz.NewAuthorizer(authz.DefaultPolicy())
	events := event.NewPostgresStore(pool)
	events.NotifyTicketsOnPublish(true)
	tickets := ticket.NewPostgresStore(pool)
	tickets.NotifyCreated(true)
	svc := ticket.NewService(tickets, events, az)
	start := time.Now().Add(48 * time.Hour)
	ev, err := events.Create(ctx, event.Event{Name: "Taslak", Location: "YTÜ", OwnerTeam: "WEBLAB", Status: event.StatusDraft, StartDate: &start})
	if err != nil {
		t.Fatal(err)
	}
	applied, err := svc.ApplyGuest(ctx, weblabLead, ev.ID, ticket.GuestInfo{FirstName: "Ada", LastName: "Lovelace", Email: "ada@example.com"})
	if err != nil || applied.Result != ticket.GuestCreated {
		t.Fatalf("add a guest to the draft: %+v %v", applied, err)
	}
	queued := func() int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM ticket_notifications WHERE ticket_id = $1`, applied.Ticket.ID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if queued() != 1 {
		t.Fatal("the guest's Ticket queued no row")
	}
	// The worker drops a draft's row (docs/ticket-created-notification.md).
	if _, err := pool.Exec(ctx, `DELETE FROM ticket_notifications`); err != nil {
		t.Fatal(err)
	}
	if _, err := event.NewService(events, az).Publish(ctx, weblabLead, ev.ID, true); err != nil {
		t.Fatal(err)
	}
	if queued() != 1 {
		t.Fatal("the publication did not queue the guest's row")
	}
}
