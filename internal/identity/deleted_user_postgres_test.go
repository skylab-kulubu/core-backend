package identity_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/identity"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
	"github.com/skylab-kulubu/core-backend/internal/testpostgres"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

func TestGetUserAnswersDeletedThroughTheWholeErasureOnPostgres(t *testing.T) {
	pool := testpostgres.Start(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	store := user.NewPostgresStore(pool)
	dir := identity.NewMemory()
	svc := identity.NewService(dir, store, authz.NewAuthorizer(authz.DefaultPolicy()))
	id := uuid.New()
	dir.PutUser(identity.Person{ID: id, Email: "ada@example.test", FirstName: "Ada", LastName: "Lovelace"})
	if _, _, err := user.NewService(store).Ensure(ctx, id, user.Profile{Email: "ada@example.test", FirstName: "Ada", LastName: "Lovelace"}); err != nil {
		t.Fatal(err)
	}
	read := func(stage string, want user.ReadStatus) {
		t.Helper()
		card, err := svc.GetUser(ctx, formsReader(), id)
		if err != nil {
			t.Fatalf("%s: %v", stage, err)
		}
		if card.Status != want {
			t.Fatalf("%s: card = %+v", stage, card)
		}
		if want != user.ReadStatusActive {
			assertErasedCard(t, card, id, want)
		}
	}
	read("active", user.ReadStatusActive)

	request, err := store.RequestDeletion(ctx, id, nil)
	if err != nil {
		t.Fatal(err)
	}
	read("deletion pending", user.ReadStatusDeletionPending)

	if err := store.AnonymizeAccount(ctx, id, time.Now().UTC(), nil); err != nil {
		t.Fatal(err)
	}
	read("anonymized", user.ReadStatusDeleted)

	if err := dir.DeleteUser(ctx, id); err != nil {
		t.Fatal(err)
	}
	read("identity deleted", user.ReadStatusDeleted)

	if _, err := pool.Exec(ctx, `UPDATE account_deletion_requests SET status = 'completed', completed_at = now() WHERE id = $1`, request.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.HardPurgeAccount(ctx, id); err != nil {
		t.Fatal(err)
	}
	read("hard-purged", user.ReadStatusDeleted)

	if _, err := svc.GetUser(ctx, formsReader(), uuid.New()); !errors.Is(err, identity.ErrNotFound) {
		t.Fatalf("unknown subject err = %v", err)
	}
}
