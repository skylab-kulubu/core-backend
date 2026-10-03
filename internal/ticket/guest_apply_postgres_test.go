package ticket_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
	"github.com/skylab-kulubu/core-backend/internal/testpostgres"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
)

// staleGuestRead answers the first guest lookup as if no Ticket existed: the
// moment between two concurrent applications' reads and writes.
type staleGuestRead struct {
	*ticket.PostgresStore
	missed atomic.Bool
}

func (s *staleGuestRead) GetByGuestEvent(ctx context.Context, email string, eventID uuid.UUID) (ticket.Ticket, error) {
	if s.missed.CompareAndSwap(false, true) {
		return ticket.Ticket{}, ticket.ErrNotFound
	}
	return s.PostgresStore.GetByGuestEvent(ctx, email, eventID)
}

func TestApplyGuestTwiceAtOnceIsOneTicket(t *testing.T) {
	pool := testpostgres.Start(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	events := event.NewPostgresStore(pool)
	ev, err := events.Create(ctx, event.Event{Name: "Hack", Location: "YTÜ", OwnerTeam: "WEBLAB"})
	if err != nil {
		t.Fatal(err)
	}
	store := ticket.NewPostgresStore(pool)
	guest := ticket.GuestInfo{FirstName: "Ada", LastName: "Lovelace", Email: "ada@example.com"}
	if _, err := store.Create(ctx, ticket.Ticket{
		EventID: ev.ID, TicketType: ticket.Guest, GuestFirstName: "Ada", GuestLastName: "Lovelace", GuestEmail: "ada@example.com",
	}); err != nil {
		t.Fatal(err)
	}

	// The store refuses the second row for the e-mail as a conflict, not as
	// an unexplained database error.
	_, err = store.Create(ctx, ticket.Ticket{
		EventID: ev.ID, TicketType: ticket.Guest, GuestFirstName: "Ada", GuestLastName: "Lovelace", GuestEmail: "ada@example.com",
	})
	if !errors.Is(err, ticket.ErrConflict) {
		t.Fatalf("second guest row: %v, want ErrConflict", err)
	}

	// The application that lost the race finds the Ticket the other wrote.
	svc := ticket.NewService(&staleGuestRead{PostgresStore: store}, events, authz.NewAuthorizer(authz.DefaultPolicy()))
	for name, caller := range map[string]authz.Principal{"anonymous": {}, "operator": weblabLead} {
		store := &staleGuestRead{PostgresStore: store}
		svc = ticket.NewService(store, events, authz.NewAuthorizer(authz.DefaultPolicy()))
		applied, err := svc.ApplyGuest(ctx, caller, ev.ID, guest)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if applied.Result != ticket.GuestExisting || !store.missed.Load() {
			t.Fatalf("%s: applied %+v", name, applied)
		}
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM tickets WHERE event_id = $1`, ev.ID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("%d guest Tickets, want 1", n)
	}
}
