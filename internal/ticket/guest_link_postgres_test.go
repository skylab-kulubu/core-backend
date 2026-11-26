package ticket_test

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
	"github.com/skylab-kulubu/core-backend/internal/testpostgres"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// plainStore hides the capacity store's CreateAdmitted: the service then
// writes with Create, the store's other insert.
type plainStore struct{ ticket.Store }

// The lookup says "linked" (its cached switch is on) but the switch was
// turned off since: the insert itself reads the switch and writes the
// Ticket unlinked, without the mark, on both inserts (CreateAdmitted,
// under the capacity, and Create). On, the same write is linked.
func TestInsertReadsTheSwitch(t *testing.T) {
	pool := testpostgres.Start(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	ada := uuid.New()
	if _, _, err := user.NewService(user.NewPostgresStore(pool)).Ensure(ctx, ada, user.Profile{Email: "ada@example.com"}); err != nil {
		t.Fatal(err)
	}
	events := event.NewPostgresStore(pool)
	owners := &fakeGuestOwners{owners: map[string]uuid.UUID{}}
	store := ticket.NewPostgresStore(pool)
	services := map[string]ticket.Service{
		"CreateAdmitted": ticket.NewService(store, events, authz.NewAuthorizer(authz.DefaultPolicy()), ticket.GuestOwners(owners)),
		"Create":         ticket.NewService(plainStore{store}, events, authz.NewAuthorizer(authz.DefaultPolicy()), ticket.GuestOwners(owners)),
	}
	written := func(svc ticket.Service, email string) (*uuid.UUID, *string) {
		t.Helper()
		ev, err := events.Create(ctx, event.Event{Name: "Hack", Location: "YTÜ", OwnerTeam: "WEBLAB"})
		if err != nil {
			t.Fatal(err)
		}
		owners.mu.Lock()
		owners.owners[email] = ada
		owners.mu.Unlock()
		if _, err := svc.ApplyGuest(ctx, authz.Principal{}, ev.ID, ticket.GuestInfo{FirstName: "Ada", LastName: "Byron", Email: email}); err != nil {
			t.Fatal(err)
		}
		var owner *uuid.UUID
		var source *string
		if err := pool.QueryRow(ctx, `SELECT owner_id, owner_link_source FROM tickets WHERE event_id = $1 AND guest_email = $2`, ev.ID, email).
			Scan(&owner, &source); err != nil {
			t.Fatal(err)
		}
		return owner, source
	}
	for path, svc := range services {
		if _, err := pool.Exec(ctx, `UPDATE guest_account_link_settings SET enabled_at = NULL`); err != nil {
			t.Fatal(err)
		}
		if owner, source := written(svc, "off-"+strings.ToLower(path)+"@kisisel.org"); owner != nil || source != nil {
			t.Fatalf("%s while off: owner %v source %v", path, owner, source)
		}
		if _, err := pool.Exec(ctx, `UPDATE guest_account_link_settings SET enabled_at = now()`); err != nil {
			t.Fatal(err)
		}
		if owner, source := written(svc, "on-"+strings.ToLower(path)+"@kisisel.org"); owner == nil || *owner != ada || source == nil || *source != "write" {
			t.Fatalf("%s while on: owner %v source %v", path, owner, source)
		}
	}
}
