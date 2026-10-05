package migrate_test

import (
	"bytes"
	"context"
	"io/fs"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/skylab-kulubu/core-backend/db"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

const (
	contactConsentsVersion     = "20261005100000"
	contactConsentsStepVersion = "20261005100100"
	// productionHeadVersion is the newest migration production had applied
	// when the contact consents were written (the guest Answer file).
	productionHeadVersion = "20261004120000"
)

func readDown(t *testing.T, name string) string {
	t.Helper()
	body, err := fs.ReadFile(db.DownSQL, "migrations/"+name)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func contactConsentsHold(t *testing.T, pool *pgxpool.Pool) (table, step bool) {
	t.Helper()
	if err := pool.QueryRow(context.Background(), `SELECT
		to_regclass('public.contact_consents') IS NOT NULL,
		EXISTS (
			SELECT 1 FROM pg_constraint
			WHERE conrelid = to_regclass('public.account_deletion_steps')
			  AND conname = 'account_deletion_steps_step_check'
			  AND pg_get_constraintdef(oid) LIKE '%''erase_contact_consents''%'
		)`).Scan(&table, &step); err != nil {
		t.Fatal(err)
	}
	return table, step
}

func recorded(t *testing.T, pool *pgxpool.Pool, version string) bool {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM schema_migrations WHERE version = `+version).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n == 1
}

// Apply runs every embedded version schema_migrations lacks, in version
// order. The consent migrations are numbered after production's newest, so
// on production's database they run last, in the order a fresh database runs
// them, and none is passed over as older than what was applied.
func TestContactConsentMigrationsApplyOnTopOfProduction(t *testing.T) {
	versions, err := migrate.Versions()
	if err != nil {
		t.Fatal(err)
	}
	head := slices.Index(versions, 20261004120000)
	table := slices.Index(versions, 20261005100000)
	step := slices.Index(versions, 20261005100100)
	if head < 0 || table < 0 || step < 0 || !(head < table && table < step) {
		t.Fatalf("versions %v: want %s < %s < %s", versions, productionHeadVersion, contactConsentsVersion, contactConsentsStepVersion)
	}

	pool := postgresPool(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	// Production's database: everything up to the guest Answer file, no
	// contact consents.
	for _, down := range []string{
		contactConsentsStepVersion + "_account_erasure_contact_consents_step.down.sql",
		contactConsentsVersion + "_contact_consents.down.sql",
	} {
		if _, err := pool.Exec(ctx, readDown(t, down)); err != nil {
			t.Fatalf("%s: %v", down, err)
		}
	}
	if _, err := pool.Exec(ctx, `DELETE FROM schema_migrations WHERE version IN (`+contactConsentsVersion+`, `+contactConsentsStepVersion+`)`); err != nil {
		t.Fatal(err)
	}
	if table, step := contactConsentsHold(t, pool); table || step || !recorded(t, pool, productionHeadVersion) {
		t.Fatalf("not production's schema: table=%v step=%v head recorded=%v", table, step, recorded(t, pool, productionHeadVersion))
	}

	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if table, step := contactConsentsHold(t, pool); !table || !step {
		t.Fatalf("after Apply table=%v step=%v", table, step)
	}
	if !recorded(t, pool, contactConsentsVersion) || !recorded(t, pool, contactConsentsStepVersion) {
		t.Fatal("the consent migrations were not recorded")
	}
	var applied []int64
	rows, err := pool.Query(ctx, `SELECT version FROM schema_migrations WHERE version IN (`+
		productionHeadVersion+`, `+contactConsentsVersion+`, `+contactConsentsStepVersion+`) ORDER BY applied_at, version`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var v int64
		if err := rows.Scan(&v); err != nil {
			t.Fatal(err)
		}
		applied = append(applied, v)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(applied, []int64{20261004120000, 20261005100000, 20261005100100}) {
		t.Fatalf("applied in order %v", applied)
	}
	// A second Apply changes nothing.
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
}

// The down migrations refuse while they would drop proof: a consent record,
// or an erasure checkpoint of the consent step. Once nothing is left they
// roll back, and Apply brings both back.
func TestContactConsentDownMigrationsRefuseToDropProof(t *testing.T) {
	pool := postgresPool(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	tableDown := readDown(t, contactConsentsVersion+"_contact_consents.down.sql")
	stepDown := readDown(t, contactConsentsStepVersion+"_account_erasure_contact_consents_step.down.sql")

	consentID := uuid.New()
	if _, err := pool.Exec(ctx, `
		INSERT INTO contact_consents (id, purpose, email_hmac, source, text_version, granted_at, ended_at, ended_reason, ended_via)
		VALUES ($1, 'event_invitations', $2, 'guest_apply', 'davet-v1', $3, $3, 'withdrawn', 'link')`,
		consentID, bytes.Repeat([]byte{1}, 32), time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, tableDown); err == nil || !strings.Contains(err.Error(), "refusing to drop") {
		t.Fatalf("down with a consent record: %v", err)
	}

	store := user.NewPostgresStore(pool)
	subjectID := uuid.New()
	if _, _, err := user.NewService(store).Ensure(ctx, subjectID, user.Profile{Email: "consent-step@example.test"}); err != nil {
		t.Fatal(err)
	}
	request, err := store.RequestDeletion(ctx, subjectID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO account_deletion_steps (request_id, step) VALUES ($1, 'erase_contact_consents')`, request.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, stepDown); err == nil || !strings.Contains(err.Error(), "completion proof") {
		t.Fatalf("down with a consent step checkpoint: %v", err)
	}
	if table, step := contactConsentsHold(t, pool); !table || !step {
		t.Fatal("a refused down changed the schema")
	}

	if _, err := pool.Exec(ctx, `DELETE FROM account_deletion_steps WHERE step = 'erase_contact_consents'`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM contact_consents`); err != nil {
		t.Fatal(err)
	}
	for _, down := range []string{stepDown, tableDown} {
		if _, err := pool.Exec(ctx, down); err != nil {
			t.Fatalf("down once nothing is left: %v", err)
		}
	}
	if table, step := contactConsentsHold(t, pool); table || step {
		t.Fatalf("after down table=%v step=%v", table, step)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM schema_migrations WHERE version IN (`+contactConsentsVersion+`, `+contactConsentsStepVersion+`)`); err != nil {
		t.Fatal(err)
	}
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if table, step := contactConsentsHold(t, pool); !table || !step {
		t.Fatal("applying again after down did not bring the contact consents back")
	}
}
