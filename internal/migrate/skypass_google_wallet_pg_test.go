package migrate_test

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

const (
	skypassWalletVersion     = "20261008120000"
	skypassWalletStepVersion = "20261008120100"
)

func skypassWalletHolds(t *testing.T, pool *pgxpool.Pool) (table, step bool) {
	t.Helper()
	if err := pool.QueryRow(context.Background(), `SELECT
		to_regclass('public.skypass_google_wallet_passes') IS NOT NULL,
		EXISTS (
			SELECT 1 FROM pg_constraint
			WHERE conrelid = to_regclass('public.account_deletion_steps')
			  AND conname = 'account_deletion_steps_step_check'
			  AND pg_get_constraintdef(oid) LIKE '%''erase_skypass_wallet''%'
		)`).Scan(&table, &step); err != nil {
		t.Fatal(err)
	}
	return table, step
}

// The Wallet migrations come after every earlier one and keep the contact
// consent step in the erasure step list.
func TestSkyPassWalletMigrationsComeLast(t *testing.T) {
	versions, err := migrate.Versions()
	if err != nil {
		t.Fatal(err)
	}
	before := slices.Index(versions, 20261007120100)
	table := slices.Index(versions, 20261008120000)
	step := slices.Index(versions, 20261008120100)
	if before < 0 || table < 0 || step < 0 || !(before < table && table < step) {
		t.Fatalf("versions %v", versions)
	}
	pool := postgresPool(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if table, step := skypassWalletHolds(t, pool); !table || !step {
		t.Fatalf("table=%v step=%v", table, step)
	}
	if consents, step := contactConsentsHold(t, pool); !consents || !step {
		t.Fatalf("the consent step left the list: table=%v step=%v", consents, step)
	}
}

// The down migrations refuse while they would lose something: a pass (the
// only record of which Google objects carry a name), or an erasure
// checkpoint of the Wallet step. Once nothing is left they roll back, and
// Apply brings both back.
func TestSkyPassWalletDownMigrationsRefuseToLoseRecords(t *testing.T) {
	pool := postgresPool(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	tableDown := readDown(t, skypassWalletVersion+"_skypass_google_wallet_passes.down.sql")
	stepDown := readDown(t, skypassWalletStepVersion+"_account_erasure_skypass_wallet_step.down.sql")

	store := user.NewPostgresStore(pool)
	holder := uuid.New()
	if _, _, err := user.NewService(store).Ensure(ctx, holder, user.Profile{Email: "wallet-holder@example.test"}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO skypass_google_wallet_passes (pass_id, user_id) VALUES ('ABCDEFGHIJKLMNOPQRSTUVWXYZ', $1)`, holder); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, tableDown); err == nil || !strings.Contains(err.Error(), "refusing to drop") {
		t.Fatalf("down with a pass: %v", err)
	}

	request, err := store.RequestDeletion(ctx, holder, nil)
	if err != nil {
		t.Fatal(err)
	}
	// No new pass for a person being erased.
	if _, err := pool.Exec(ctx, `INSERT INTO skypass_google_wallet_passes (pass_id, user_id, revoked_at) VALUES ('BBBBBBBBBBBBBBBBBBBBBBBBBB', $1, now())`, holder); err == nil ||
		!strings.Contains(err.Error(), "not active") {
		t.Fatalf("pass for a person being erased: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO account_deletion_steps (request_id, step) VALUES ($1, 'erase_skypass_wallet')`, request.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, stepDown); err == nil || !strings.Contains(err.Error(), "completion proof") {
		t.Fatalf("down with a wallet step checkpoint: %v", err)
	}
	if table, step := skypassWalletHolds(t, pool); !table || !step {
		t.Fatal("a refused down changed the schema")
	}

	if _, err := pool.Exec(ctx, `DELETE FROM account_deletion_steps WHERE step = 'erase_skypass_wallet'`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM skypass_google_wallet_passes`); err != nil {
		t.Fatal(err)
	}
	for _, down := range []string{stepDown, tableDown} {
		if _, err := pool.Exec(ctx, down); err != nil {
			t.Fatalf("down once nothing is left: %v", err)
		}
	}
	if table, step := skypassWalletHolds(t, pool); table || step {
		t.Fatalf("after down table=%v step=%v", table, step)
	}
	if consents, step := contactConsentsHold(t, pool); !consents || !step {
		t.Fatal("the Wallet step's down dropped the consent step")
	}
	if _, err := pool.Exec(ctx, `DELETE FROM schema_migrations WHERE version IN (`+skypassWalletVersion+`, `+skypassWalletStepVersion+`)`); err != nil {
		t.Fatal(err)
	}
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if table, step := skypassWalletHolds(t, pool); !table || !step {
		t.Fatal("applying again after down did not bring the Wallet passes back")
	}
}
