package ticket_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

func TestService_ListByEventAnswersTheOwnerStatus(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	events := event.NewMemoryStore()
	users := user.NewMemoryStore()
	tickets := ticket.NewMemoryStore()
	svc := ticket.NewService(tickets, events, authz.NewAuthorizer(authz.DefaultPolicy()), users, unavailablePersonReader{})
	ev := seedEvent(t, events, "WEBLAB")
	active, pending := uuid.New(), uuid.New()
	seedUser(t, users, active)
	if _, _, err := user.NewService(users).Ensure(ctx, pending, user.Profile{
		Email: "grace@example.com", FirstName: "Grace", LastName: "Hopper",
	}); err != nil {
		t.Fatal(err)
	}
	for _, owner := range []uuid.UUID{active, pending} {
		if _, err := tickets.Create(ctx, ticket.Ticket{EventID: ev.ID, TicketType: ticket.Registered, OwnerID: &owner}); err != nil {
			t.Fatal(err)
		}
	}
	// Erasure detaches the ticket (owner_id NULL) when it anonymizes the
	// person, so only a deletion-pending owner is ever embedded.
	if _, err := users.RequestDeletion(ctx, pending, nil); err != nil {
		t.Fatal(err)
	}

	leader := authz.Principal{ID: "lead", Groups: []string{"/UYELER/ARGE/WEBLAB/LIDERLER"}}
	rows, err := svc.ListByEvent(ctx, leader, ev.ID)
	if err != nil {
		t.Fatal(err)
	}
	got := map[uuid.UUID]ticket.PersonSummary{}
	for _, row := range rows {
		if row.Owner == nil {
			t.Fatalf("owner missing: %+v", row)
		}
		got[*row.OwnerID] = *row.Owner
	}
	if want := (ticket.PersonSummary{
		ID: active, Email: "ada@example.com", FirstName: "Ada", LastName: "Lovelace",
		Status: user.ReadStatusActive, DisplayName: "Ada Lovelace",
	}); got[active] != want {
		t.Fatalf("active owner = %+v", got[active])
	}
	if want := (ticket.PersonSummary{
		ID: pending, FirstName: user.DeletedDisplayName,
		Status: user.ReadStatusDeletionPending, DisplayName: user.DeletedDisplayName,
	}); got[pending] != want {
		t.Fatalf("deletion-pending owner = %+v", got[pending])
	}
}
