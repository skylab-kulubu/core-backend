package teampagenotice_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
	"github.com/skylab-kulubu/core-backend/internal/teampagenotice"
	"github.com/skylab-kulubu/core-backend/internal/testpostgres"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// The ledger claims a person once, releases a claim, and loses the
// person's row when their account is erased.
func TestPostgresLedgerAndAccountErasure(t *testing.T) {
	pool := testpostgres.Start(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	l := teampagenotice.NewPostgresLedger(pool)
	now := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	ada, grace := uuid.New(), uuid.New()
	for _, id := range []uuid.UUID{ada, grace} {
		if got, err := l.Claim(ctx, id, now); err != nil || !got {
			t.Fatalf("claim: %v %v", got, err)
		}
	}
	if got, err := l.Claim(ctx, ada, now); err != nil || got {
		t.Fatalf("second claim: %v %v", got, err)
	}
	if err := l.Release(ctx, grace); err != nil {
		t.Fatal(err)
	}
	notified, err := l.Notified(ctx, []uuid.UUID{ada, grace})
	if err != nil || !notified[ada] || notified[grace] {
		t.Fatalf("notified %v %v", notified, err)
	}
	if empty, err := l.Notified(ctx, nil); err != nil || len(empty) != 0 {
		t.Fatalf("none asked: %v %v", empty, err)
	}

	users := user.NewPostgresStore(pool)
	if _, _, err := user.NewService(users).Ensure(ctx, ada, user.Profile{Email: "ada@example.test"}); err != nil {
		t.Fatal(err)
	}
	if _, err := users.RequestDeletion(ctx, ada, nil); err != nil {
		t.Fatal(err)
	}
	if err := users.AnonymizeAccount(ctx, ada, now, nil); err != nil {
		t.Fatal(err)
	}
	if notified, err := l.Notified(ctx, []uuid.UUID{ada}); err != nil || notified[ada] {
		t.Fatalf("after erasure %v %v", notified, err)
	}
}
