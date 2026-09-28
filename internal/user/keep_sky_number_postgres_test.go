package user_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/skylab-kulubu/core-backend/internal/migrate"
	"github.com/skylab-kulubu/core-backend/internal/testpostgres"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// A stored Sky number is never replaced by the upsert: of two concurrent
// first requests that both assigned one, the first stored stays, and the
// upsert answers it, so the second writes that number back to Keycloak and
// not its own.
func TestUpsertNeverReplacesAStoredSkyNumber(t *testing.T) {
	pool := testpostgres.Start(t)
	if err := migrate.Apply(context.Background(), pool); err != nil {
		t.Fatal(err)
	}
	for name, store := range map[string]user.Store{"postgres": user.NewPostgresStore(pool), "memory": user.NewMemoryStore()} {
		ctx := context.Background()
		id := uuid.New()
		if _, _, err := store.Upsert(ctx, user.User{ID: id, Email: name + "@example.com"}); err != nil {
			t.Fatal(err)
		}
		if _, _, err := store.Upsert(ctx, user.User{ID: id, Email: name + "@example.com", SkyNumber: "SKY-0000007"}); err != nil {
			t.Fatal(err)
		}
		got, created, err := store.Upsert(ctx, user.User{ID: id, Email: name + "@example.com", SkyNumber: "SKY-0000008"})
		if err != nil {
			t.Fatal(err)
		}
		if created || got.SkyNumber != "SKY-0000007" {
			t.Errorf("%s: upsert answered %q (created %v), want the stored SKY-0000007", name, got.SkyNumber, created)
		}
		if stored, err := store.Get(ctx, id); err != nil || stored.SkyNumber != "SKY-0000007" {
			t.Errorf("%s: stored %q (%v), want SKY-0000007", name, stored.SkyNumber, err)
		}
	}
}
