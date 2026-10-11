package identity_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/identity"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
	"github.com/skylab-kulubu/core-backend/internal/testpostgres"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// Reading the unsearched full list leaves the stored rows as they are on
// Postgres, where Ensure's write sets updated_at = now(): only the person
// core has no row for is written, and a second read writes nobody.
func TestService_ListUsersLeavesStoredRowsUnwrittenOnPostgres(t *testing.T) {
	ctx := context.Background()
	pool := testpostgres.Start(t)
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	store := user.NewPostgresStore(pool)
	dir := identity.NewMemory()
	svc := identity.NewService(dir, store, authz.NewAuthorizer(authz.DefaultPolicy()))

	stored := make([]uuid.UUID, 0, 3)
	for _, first := range []string{"Ada", "Grace", "Barbara"} {
		id := uuid.New()
		person := identity.Person{ID: id, Email: id.String() + "@example.test", FirstName: first, LastName: "Keycloak", Username: id.String()}
		dir.PutUser(person)
		if _, _, err := user.NewService(store).Ensure(ctx, id, user.Profile{
			Email: person.Email, FirstName: first, LastName: "Core", Username: person.Username,
		}); err != nil {
			t.Fatal(err)
		}
		stored = append(stored, id)
	}
	missing := uuid.New()
	dir.PutUser(identity.Person{ID: missing, Email: missing.String() + "@example.test", FirstName: "Lin", LastName: "Keycloak", Username: missing.String()})

	stamps := func() map[uuid.UUID]time.Time {
		t.Helper()
		out := map[uuid.UUID]time.Time{}
		rows, err := pool.Query(ctx, `SELECT id, updated_at FROM users`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		for rows.Next() {
			var id uuid.UUID
			var at time.Time
			if err := rows.Scan(&id, &at); err != nil {
				t.Fatal(err)
			}
			out[id] = at
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return out
	}

	before := stamps()
	people, err := svc.ListUsers(ctx, privileged(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(people) != 4 {
		t.Fatalf("people = %+v", people)
	}
	after := stamps()
	for _, id := range stored {
		if !after[id].Equal(before[id]) {
			t.Fatalf("the list rewrote stored row %s: updated_at %v -> %v", id, before[id], after[id])
		}
	}
	created, err := store.Get(ctx, missing)
	if err != nil {
		t.Fatalf("the list did not store the missing person: %v", err)
	}
	if created.SkyNumber == "" {
		t.Fatalf("missing person stored without a Sky number: %+v", created)
	}
	for _, p := range people {
		if p.ID == missing && p.SkyNumber != created.SkyNumber {
			t.Fatalf("the list answered %q, stored %q", p.SkyNumber, created.SkyNumber)
		}
		if p.ID == stored[0] && p.FirstName != "Ada" {
			t.Fatalf("the list lost core's profile: %+v", p)
		}
	}

	if _, err := svc.ListUsers(ctx, privileged(), ""); err != nil {
		t.Fatal(err)
	}
	again := stamps()
	if len(again) != len(after) {
		t.Fatalf("rows %d -> %d", len(after), len(again))
	}
	for id, at := range after {
		if !again[id].Equal(at) {
			t.Fatalf("the second list wrote %s: updated_at %v -> %v", id, at, again[id])
		}
	}
}
