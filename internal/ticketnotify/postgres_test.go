package ticketnotify_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
	"github.com/skylab-kulubu/core-backend/internal/testpostgres"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
	"github.com/skylab-kulubu/core-backend/internal/ticketnotify"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

func migrated(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool := testpostgres.Start(t)
	if err := migrate.Apply(context.Background(), pool); err != nil {
		t.Fatal(err)
	}
	return pool
}

func queued(t *testing.T, pool *pgxpool.Pool) []uuid.UUID {
	t.Helper()
	rows, err := pool.Query(context.Background(), `SELECT ticket_id FROM ticket_notifications ORDER BY created_at, ticket_id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		out = append(out, id)
	}
	return out
}

// The Ticket's insert queues it while the Notification is on, and only then;
// a refused insert (the owner's second Ticket) queues nothing.
func TestTicketInsertQueuesItsNotification(t *testing.T) {
	pool := migrated(t)
	ctx := context.Background()
	ev, err := event.NewPostgresStore(pool).Create(ctx, event.Event{Name: "Hack", Location: "YTÜ", OwnerTeam: "WEBLAB"})
	if err != nil {
		t.Fatal(err)
	}
	store := ticket.NewPostgresStore(pool)
	off, err := store.Create(ctx, ticket.Ticket{EventID: ev.ID, TicketType: ticket.Guest, GuestFirstName: "A", GuestLastName: "B", GuestEmail: "off@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	store.NotifyCreated(true)
	on, err := store.Create(ctx, ticket.Ticket{EventID: ev.ID, TicketType: ticket.Guest, GuestFirstName: "A", GuestLastName: "B", GuestEmail: "on@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if on.ID == uuid.Nil || on.GuestEmail != "on@example.com" || on.CheckIns == nil {
		t.Fatalf("created %+v", on)
	}
	if _, err := store.Create(ctx, ticket.Ticket{EventID: ev.ID, TicketType: ticket.Guest, GuestFirstName: "A", GuestLastName: "B", GuestEmail: "on@example.com"}); err != ticket.ErrConflict {
		t.Fatalf("second Ticket: %v, want ErrConflict", err)
	}
	if got := queued(t, pool); len(got) != 1 || got[0] != on.ID {
		t.Fatalf("queued %v, want only %s (not %s)", got, on.ID, off.ID)
	}
}

func TestPostgresQueueClaimsRetriesAndCompletes(t *testing.T) {
	pool := migrated(t)
	ctx := context.Background()
	ev, err := event.NewPostgresStore(pool).Create(ctx, event.Event{Name: "Hack", Location: "YTÜ", OwnerTeam: "WEBLAB"})
	if err != nil {
		t.Fatal(err)
	}
	store := ticket.NewPostgresStore(pool)
	store.NotifyCreated(true)
	tk, err := store.Create(ctx, ticket.Ticket{EventID: ev.ID, TicketType: ticket.Guest, GuestFirstName: "A", GuestLastName: "B", GuestEmail: "a@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	q := ticketnotify.NewPostgresQueue(pool)
	now := time.Now().UTC().Add(time.Second)
	claimed, err := q.Claim(ctx, now, time.Minute, 10)
	if err != nil || len(claimed) != 1 || claimed[0].TicketID != tk.ID || claimed[0].Attempts != 0 {
		t.Fatalf("claim %+v, %v", claimed, err)
	}
	// Held: a second core takes nothing during the lease.
	if again, err := q.Claim(ctx, now, time.Minute, 10); err != nil || len(again) != 0 {
		t.Fatalf("second claim %+v, %v", again, err)
	}
	if err := q.Retry(ctx, claimed[0], now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if early, err := q.Claim(ctx, now.Add(30*time.Minute), time.Minute, 10); err != nil || len(early) != 0 {
		t.Fatalf("claim before the retry is due %+v, %v", early, err)
	}
	later, err := q.Claim(ctx, now.Add(2*time.Hour), time.Minute, 10)
	if err != nil || len(later) != 1 || later[0].Attempts != 1 {
		t.Fatalf("claim after the retry %+v, %v", later, err)
	}
	// A stale claim's write changes nothing.
	if err := q.Complete(ctx, claimed[0]); err != nil {
		t.Fatal(err)
	}
	if len(queued(t, pool)) != 1 {
		t.Fatal("a stale claim completed the row")
	}
	if err := q.Complete(ctx, later[0]); err != nil {
		t.Fatal(err)
	}
	if got := queued(t, pool); len(got) != 0 {
		t.Fatalf("queued %v after complete", got)
	}
}

func TestAccountErasureForgetsQueuedTicketNotifications(t *testing.T) {
	pool := migrated(t)
	ctx := context.Background()
	users := user.NewPostgresStore(pool)
	subject, other := uuid.New(), uuid.New()
	for _, id := range []uuid.UUID{subject, other} {
		if _, _, err := user.NewService(users).Ensure(ctx, id, user.Profile{Email: id.String() + "@example.test"}); err != nil {
			t.Fatal(err)
		}
	}
	ev, err := event.NewPostgresStore(pool).Create(ctx, event.Event{Name: "Hack", Location: "YTÜ", OwnerTeam: "WEBLAB"})
	if err != nil {
		t.Fatal(err)
	}
	store := ticket.NewPostgresStore(pool)
	store.NotifyCreated(true)
	mine, err := store.Create(ctx, ticket.Ticket{EventID: ev.ID, TicketType: ticket.Registered, OwnerID: &subject})
	if err != nil {
		t.Fatal(err)
	}
	asGuest, err := store.Create(ctx, ticket.Ticket{EventID: ev.ID, TicketType: ticket.Guest, GuestFirstName: "A", GuestLastName: "B", GuestEmail: subject.String() + "@example.test"})
	if err != nil {
		t.Fatal(err)
	}
	theirs, err := store.Create(ctx, ticket.Ticket{EventID: ev.ID, TicketType: ticket.Registered, OwnerID: &other})
	if err != nil {
		t.Fatal(err)
	}
	if len(queued(t, pool)) != 3 {
		t.Fatal("three Tickets, three rows")
	}
	if _, err := users.RequestDeletion(ctx, subject, nil); err != nil {
		t.Fatal(err)
	}
	if err := users.AnonymizeAccount(ctx, subject, time.Now().UTC(), nil); err != nil {
		t.Fatal(err)
	}
	if got := queued(t, pool); len(got) != 1 || got[0] != theirs.ID {
		t.Fatalf("queued %v after erasure, want only %s (not %s, %s)", got, theirs.ID, mine.ID, asGuest.ID)
	}
}
