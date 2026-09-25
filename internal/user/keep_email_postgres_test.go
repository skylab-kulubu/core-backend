package user_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/skylab-kulubu/core-backend/internal/migrate"
	"github.com/skylab-kulubu/core-backend/internal/testpostgres"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// Account Center's access token carries no e-mail claim. A request made with it
// must not erase the address a token with the claim stored earlier.
func TestPostgresEnsureKeepsTheEmailWhenTheTokenCarriesNone(t *testing.T) {
	pool := testpostgres.Start(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	users := user.NewService(user.NewPostgresStore(pool))
	id := uuid.New()

	if _, _, err := users.Ensure(ctx, id, user.Profile{Email: "ada@example.com", FirstName: "Ada", LastName: "Lovelace", Username: "ada"}); err != nil {
		t.Fatal(err)
	}
	got, _, err := users.Ensure(ctx, id, user.Profile{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Email != "ada@example.com" {
		t.Fatalf("email after a claimless request = %q, want it kept", got.Email)
	}

	got, _, err = users.Ensure(ctx, id, user.Profile{Email: "ada@new.example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Email != "ada@new.example.com" {
		t.Fatalf("email after a token with a new address = %q, want it updated", got.Email)
	}
}
