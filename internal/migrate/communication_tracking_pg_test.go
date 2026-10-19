package migrate_test

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
)

const communicationTrackingVersion = "20261019120000"

func communicationTrackingHolds(t *testing.T, pool *pgxpool.Pool) bool {
	t.Helper()
	var holds bool
	if err := pool.QueryRow(context.Background(), `SELECT
		to_regclass('public.communication_tracking_withdrawals') IS NOT NULL
		AND to_regclass('public.communication_tracking_changes') IS NOT NULL
		AND EXISTS (
			SELECT 1 FROM pg_constraint
			WHERE conrelid = to_regclass('public.communication_preferences')
			  AND conname = 'communication_preferences_category_check'
			  AND pg_get_constraintdef(oid) LIKE '%''tracking''%'
		)`).Scan(&holds); err != nil {
		t.Fatal(err)
	}
	return holds
}

// The tracking migration comes after the preference table's and takes one
// tracking row per person, on e-mail, with no group and no kind; the
// fingerprint records it without running it again.
func TestCommunicationTrackingMigration(t *testing.T) {
	versions, err := migrate.Versions()
	if err != nil {
		t.Fatal(err)
	}
	if table, tracking := slices.Index(versions, 20261012180000), slices.Index(versions, 20261019120000); table < 0 || tracking < table {
		t.Fatalf("versions %v", versions)
	}
	pool := postgresPool(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if !communicationTrackingHolds(t, pool) {
		t.Fatal("tracking schema missing")
	}
	person := uuid.New()
	insert := func(channel string, group, kind any) error {
		_, err := pool.Exec(ctx, `INSERT INTO communication_preferences (user_id, channel, scope, group_id, news_kind, enabled, source)
			VALUES ($1, $2, 'tracking', $3, $4, false, 'account_center')`, person, channel, group, kind)
		return err
	}
	if err := insert("email", nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := insert("email", nil, nil); err == nil || !strings.Contains(err.Error(), "communication_preferences_key") {
		t.Fatalf("twice: %v", err)
	}
	for _, bad := range []struct {
		group, kind any
	}{{"g-weblab", nil}, {nil, "duyuru"}} {
		if err := insert("email", bad.group, bad.kind); err == nil || !strings.Contains(err.Error(), "communication_preferences_category_check") {
			t.Fatalf("%v: %v", bad, err)
		}
	}
	// The earlier shapes still hold.
	if _, err := pool.Exec(ctx, `INSERT INTO communication_preferences (user_id, scope, news_kind, enabled, source) VALUES ($1, 'club', 'duyuru', false, 'unsubscribe_link')`, person); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO communication_tracking_changes (user_id, enabled, source, changed_at) VALUES ($1, false, 'unsubscribe_link', now())`, person); err == nil ||
		!strings.Contains(err.Error(), "communication_tracking_changes_source_check") {
		t.Fatalf("audit from a link: %v", err)
	}

	if _, err := pool.Exec(ctx, `DELETE FROM schema_migrations WHERE version = `+communicationTrackingVersion); err != nil {
		t.Fatal(err)
	}
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	var recorded, rows int
	if err := pool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM schema_migrations WHERE version = `+communicationTrackingVersion+`),
		(SELECT count(*) FROM communication_preferences WHERE scope = 'tracking')`).Scan(&recorded, &rows); err != nil || recorded != 1 || rows != 1 {
		t.Fatalf("recorded=%d rows=%d err=%v", recorded, rows, err)
	}
}

// The down migration refuses while it would lose an objection, a waiting
// withdrawal or the audit trail; once nothing is left it rolls back to the
// earlier CHECK, and Apply brings it back.
func TestCommunicationTrackingDownRefusesToLoseRecords(t *testing.T) {
	pool := postgresPool(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	down := readDown(t, communicationTrackingVersion+"_communication_tracking.down.sql")
	person := uuid.New()
	for _, c := range []struct {
		insert, refusal, clear string
	}{
		{`INSERT INTO communication_preferences (user_id, scope, enabled, source) VALUES ($1, 'tracking', false, 'account_center')`,
			"while a person has one", `DELETE FROM communication_preferences WHERE scope = 'tracking'`},
		{`INSERT INTO communication_tracking_withdrawals (user_id, requested_at, next_attempt_at) VALUES ($1, now(), now())`,
			"waits for SkyMail", `DELETE FROM communication_tracking_withdrawals`},
		{`INSERT INTO communication_tracking_changes (user_id, enabled, source, changed_at) VALUES ($1, false, 'account_center', now())`,
			"audit trail", `DELETE FROM communication_tracking_changes`},
	} {
		if _, err := pool.Exec(ctx, c.insert, person); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, down); err == nil || !strings.Contains(err.Error(), c.refusal) {
			t.Fatalf("down with %q: %v", c.refusal, err)
		}
		if !communicationTrackingHolds(t, pool) {
			t.Fatal("a refused down changed the schema")
		}
		if _, err := pool.Exec(ctx, c.clear); err != nil {
			t.Fatal(err)
		}
	}
	// A News row stays through the down and the CHECK it returns to.
	if _, err := pool.Exec(ctx, `INSERT INTO communication_preferences (user_id, scope, enabled, source) VALUES ($1, 'all', false, 'account_center')`, person); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, down); err != nil {
		t.Fatalf("down once nothing is left: %v", err)
	}
	if communicationTrackingHolds(t, pool) {
		t.Fatal("down left the tracking schema")
	}
	if _, err := pool.Exec(ctx, `INSERT INTO communication_preferences (user_id, scope, enabled, source) VALUES ($1, 'tracking', false, 'account_center')`, uuid.New()); err == nil {
		t.Fatal("the old CHECK takes a tracking row")
	}
	if _, err := pool.Exec(ctx, `DELETE FROM schema_migrations WHERE version = `+communicationTrackingVersion); err != nil {
		t.Fatal(err)
	}
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if !communicationTrackingHolds(t, pool) {
		t.Fatal("applying again after down did not bring the tracking schema back")
	}
	var kept int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM communication_preferences WHERE user_id = $1`, person).Scan(&kept); err != nil || kept != 1 {
		t.Fatalf("kept %d %v", kept, err)
	}
}
