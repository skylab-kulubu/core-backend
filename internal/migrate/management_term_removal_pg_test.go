package migrate_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
)

const (
	managementTermRemovalVersion = "20261126101700"
	managementTermRemovalDown    = managementTermRemovalVersion + "_management_term_removal.down.sql"
)

// The removal's migration: the system basis takes member.remove (and still
// no other person's action), a run records how it treated the removal; the
// release before writes its runs on the new schema; the up migration runs
// again over itself (its fingerprint records it); down drops the runs'
// columns and keeps the audit rows, and up adds them back.
func TestManagementTermRemovalMigration(t *testing.T) {
	pool := postgresPool(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	system := `INSERT INTO team_audit_records (id, occurred_at, actor_id, actor_basis, action, group_id, group_path, target_user_id, outcome, client)
		VALUES ($1, now(), '00000000-0000-0000-0000-000000000000', 'system', $2, gen_random_uuid(), '/UYELER/YK', gen_random_uuid(), 'ok', 'system:term-removal')`
	removed := uuid.New()
	if _, err := pool.Exec(ctx, system, removed, "member.remove"); err != nil {
		t.Fatalf("the removal's row: %v", err)
	}
	for _, action := range []string{"member.add", "leader.remove", "membership.term.extend"} {
		if _, err := pool.Exec(ctx, system, uuid.New(), action); err == nil {
			t.Fatalf("a system %s was accepted", action)
		}
	}
	// The release before: a run without the removal's columns.
	oldRun := `INSERT INTO management_term_runs (id, mode, triggered_by, started_at, status) VALUES ($1, 'apply', 'schedule', now(), 'ok')`
	run := uuid.New()
	if _, err := pool.Exec(ctx, oldRun, run); err != nil {
		t.Fatal(err)
	}
	var removal string
	var count int
	if err := pool.QueryRow(ctx, `SELECT removal, removed FROM management_term_runs WHERE id = $1`, run).Scan(&removal, &count); err != nil {
		t.Fatal(err)
	}
	if removal != "off" || count != 0 {
		t.Fatalf("the release before's run: %s %d", removal, count)
	}
	if _, err := pool.Exec(ctx, `UPDATE management_term_runs SET removal = 'maybe' WHERE id = $1`, run); err == nil {
		t.Fatal("a removal of no mode was accepted")
	}

	// Again over itself: recorded, nothing changes.
	if _, err := pool.Exec(ctx, `DELETE FROM schema_migrations WHERE version = `+managementTermRemovalVersion); err != nil {
		t.Fatal(err)
	}
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if !recorded(t, pool, managementTermRemovalVersion) {
		t.Fatal("not recorded")
	}

	if _, err := pool.Exec(ctx, readDown(t, managementTermRemovalDown)); err != nil {
		t.Fatal(err)
	}
	var columns, rows int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM information_schema.columns
		WHERE table_schema = 'public' AND table_name = 'management_term_runs' AND column_name IN ('removal', 'removed')`).Scan(&columns); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM team_audit_records WHERE id = $1`, removed).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if columns != 0 || rows != 1 {
		t.Fatalf("after down: %d columns, %d rows", columns, rows)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM schema_migrations WHERE version = `+managementTermRemovalVersion); err != nil {
		t.Fatal(err)
	}
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE management_term_runs SET removal = 'apply', removed = 2 WHERE id = $1`, run); err != nil {
		t.Fatalf("up again: %v", err)
	}
}
