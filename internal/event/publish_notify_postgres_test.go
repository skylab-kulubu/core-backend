package event_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
	"github.com/skylab-kulubu/core-backend/internal/testpostgres"
)

// Publishing a draft with notifyTickets queues its Tickets' "your ticket is
// ready" Notifications in the same transaction (K3, 2026-11-22), only while
// the new Ticket's Notification is on; a row already queued stays as it is.
// Without notifyTickets, with the switch off, or when nothing is published
// (already published), nothing is queued.
func TestPublishQueuesTheDraftsTicketNotifications(t *testing.T) {
	pool := testpostgres.Start(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	store := event.NewPostgresStore(pool)
	svc := event.NewService(store, authz.NewAuthorizer(authz.DefaultPolicy()))
	start := time.Now().Add(48 * time.Hour)
	draft := func(tickets int) (event.Event, []uuid.UUID) {
		t.Helper()
		e, err := svc.Create(ctx, copyLeader, event.Event{Name: "Taslak", Location: "YTÜ", OwnerTeam: "WEBLAB", Status: event.StatusDraft, StartDate: &start})
		if err != nil {
			t.Fatal(err)
		}
		var ids []uuid.UUID
		for range tickets {
			id := uuid.New()
			if _, err := pool.Exec(ctx, `INSERT INTO tickets (id, event_id, ticket_type, guest_first_name, guest_last_name, guest_email) VALUES ($1, $2, 'GUEST', 'Ada', 'Lovelace', $3)`,
				id, e.ID, id.String()+"@example.com"); err != nil {
				t.Fatal(err)
			}
			ids = append(ids, id)
		}
		return e, ids
	}
	queued := func(eventID uuid.UUID) int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM ticket_notifications n JOIN tickets t ON t.id = n.ticket_id WHERE t.event_id = $1`, eventID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	store.NotifyTicketsOnPublish(true)
	asked, tickets := draft(3)
	// One row still queued (the worker has not passed yet): it stays, once.
	if _, err := pool.Exec(ctx, `INSERT INTO ticket_notifications (ticket_id, attempts) VALUES ($1, 2)`, tickets[0]); err != nil {
		t.Fatal(err)
	}
	if got, err := svc.Publish(ctx, copyLeader, asked.ID, true); err != nil || got.Status != event.StatusPublished {
		t.Fatalf("publish: %+v %v", got, err)
	}
	if n := queued(asked.ID); n != 3 {
		t.Fatalf("queued %d, want 3", n)
	}
	var attempts int
	if err := pool.QueryRow(ctx, `SELECT attempts FROM ticket_notifications WHERE ticket_id = $1`, tickets[0]).Scan(&attempts); err != nil || attempts != 2 {
		t.Fatalf("the row already queued: attempts %d %v", attempts, err)
	}
	// Published already: publishing again queues nothing.
	if _, err := pool.Exec(ctx, `DELETE FROM ticket_notifications`); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Publish(ctx, copyLeader, asked.ID, true); err != nil || queued(asked.ID) != 0 {
		t.Fatalf("published again: %d %v", queued(asked.ID), err)
	}

	notAsked, _ := draft(2)
	if _, err := svc.Publish(ctx, copyLeader, notAsked.ID, false); err != nil || queued(notAsked.ID) != 0 {
		t.Fatalf("not asked: %d %v", queued(notAsked.ID), err)
	}

	store.NotifyTicketsOnPublish(false)
	switchOff, _ := draft(2)
	if _, err := svc.Publish(ctx, copyLeader, switchOff.ID, true); err != nil || queued(switchOff.ID) != 0 {
		t.Fatalf("switch off: %d %v", queued(switchOff.ID), err)
	}

	// A draft that cannot be published (no start date) queues nothing.
	store.NotifyTicketsOnPublish(true)
	undated, err := svc.Create(ctx, copyLeader, event.Event{Name: "Tarihsiz", Location: "YTÜ", OwnerTeam: "WEBLAB", Status: event.StatusDraft})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO tickets (id, event_id, ticket_type, guest_email) VALUES ($1, $2, 'GUEST', 'grace@example.com')`, uuid.New(), undated.ID); err != nil {
		t.Fatal(err)
	}
	if changed, err := store.Publish(ctx, undated.ID, nil, true); err != nil || changed || queued(undated.ID) != 0 {
		t.Fatalf("undated: %v %v %d", changed, err, queued(undated.ID))
	}
}
