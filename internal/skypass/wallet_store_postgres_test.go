package skypass

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
	"github.com/skylab-kulubu/core-backend/internal/testpostgres"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// The store against PostgreSQL: one active pass per person, a counter that
// only moves forward (one use per code, also under concurrent scans), and
// the account reference guard.
func TestPostgresWalletStore(t *testing.T) {
	pool := testpostgres.Start(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	users := user.NewPostgresStore(pool)
	ada := uuid.New()
	if _, _, err := user.NewService(users).Ensure(ctx, ada, user.Profile{Email: "wallet-ada@example.test", FirstName: "Ada"}); err != nil {
		t.Fatal(err)
	}
	store := NewPostgresWalletStore(pool)
	at := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)

	if _, ok, err := store.ActivePass(ctx, ada); ok || err != nil {
		t.Fatalf("active before %v %v", ok, err)
	}
	first, err := store.CreatePass(ctx, ada, "AAAAAAAAAAAAAAAAAAAAAAAAAA", at)
	if err != nil || first.PassID != "AAAAAAAAAAAAAAAAAAAAAAAAAA" || first.UserID != ada || first.LastCounter != 0 || first.RevokedAt != nil {
		t.Fatalf("create %+v %v", first, err)
	}
	// A second request finds the first one's pass.
	again, err := store.CreatePass(ctx, ada, "BBBBBBBBBBBBBBBBBBBBBBBBBB", at)
	if err != nil || again.PassID != first.PassID {
		t.Fatalf("second create %+v %v", again, err)
	}
	// The pass id format is the database's too.
	grace := uuid.New()
	if _, _, err := user.NewService(users).Ensure(ctx, grace, user.Profile{Email: "wallet-grace@example.test", FirstName: "Grace"}); err != nil {
		t.Fatal(err)
	}
	_, err = pool.Exec(ctx, `INSERT INTO skypass_google_wallet_passes (pass_id, user_id) VALUES ('lower-case', $1)`, grace)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.ConstraintName != "skypass_google_wallet_passes_pass_id_check" {
		t.Fatalf("a malformed pass id: %v", err)
	}

	if ok, err := store.ConsumeCounter(ctx, first.PassID, 100); !ok || err != nil {
		t.Fatalf("consume %v %v", ok, err)
	}
	for _, counter := range []int64{100, 99} {
		if ok, err := store.ConsumeCounter(ctx, first.PassID, counter); ok || err != nil {
			t.Fatalf("consume %d again %v %v", counter, ok, err)
		}
	}
	// Two scans of the same new code at once: one wins.
	var wg sync.WaitGroup
	wins := make(chan bool, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, err := store.ConsumeCounter(ctx, first.PassID, 101)
			if err != nil {
				t.Error(err)
			}
			wins <- ok
		}()
	}
	wg.Wait()
	close(wins)
	won := 0
	for ok := range wins {
		if ok {
			won++
		}
	}
	if won != 1 {
		t.Fatalf("%d scans spent the same code", won)
	}

	if err := store.Revoke(ctx, first.PassID, at.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	got, ok, err := store.PassByID(ctx, first.PassID)
	if !ok || err != nil || got.RevokedAt == nil || got.LastCounter != 101 {
		t.Fatalf("revoked %+v %v %v", got, ok, err)
	}
	if ok, err := store.ConsumeCounter(ctx, first.PassID, 200); ok || err != nil {
		t.Fatalf("consume on a revoked pass %v %v", ok, err)
	}
	// After a revocation the person may have a new pass.
	second, err := store.CreatePass(ctx, ada, "CCCCCCCCCCCCCCCCCCCCCCCCCC", at.Add(2*time.Minute))
	if err != nil || second.PassID != "CCCCCCCCCCCCCCCCCCCCCCCCCC" {
		t.Fatalf("new pass %+v %v", second, err)
	}
	passes, err := store.PassesOf(ctx, ada)
	if err != nil || len(passes) != 2 || passes[0].PassID != first.PassID || passes[1].PassID != second.PassID {
		t.Fatalf("passes %+v %v", passes, err)
	}
	if err := store.Delete(ctx, first.PassID); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := store.PassByID(ctx, first.PassID); ok {
		t.Fatal("deleted pass still there")
	}

	// A person being erased gets no new pass.
	if _, err := users.RequestDeletion(ctx, ada, nil); err != nil {
		t.Fatal(err)
	}
	if err := store.Revoke(ctx, second.PassID, at); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreatePass(ctx, ada, "DDDDDDDDDDDDDDDDDDDDDDDDDD", at); !errors.Is(err, ErrNotFound) {
		t.Fatalf("pass for a person being erased: %v", err)
	}
}
