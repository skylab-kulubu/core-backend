package migrate_test

import (
	"context"
	"io/fs"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/db"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
)

const retentionRunsVersion = "20261005120000"

// The destruction records hold no personal data: every column is one of
// these, and none names an address, a person, an IP or a row of another
// table.
var retentionRecordColumns = map[string]string{
	"retention_periods":   "apply_runs,closed_at,dry_runs,ends_at,id,rows_changed,started_at",
	"retention_runs":      "allow_large,error_code,finished_at,full_run,id,mode,period_id,rule_set_version,started_at,status,triggered_by",
	"retention_run_rules": "action,anchorless,changed,cutoff,error_code,finished_at,kind,matched,overdue,related_changed,rule,rule_version,run_id,started_at,status,table_rows,target_table",
}

// The records are created on a live database without touching an existing
// table, are recorded again without harm, keep their shape checks, and are
// forward-only once a run is recorded (kept three years).
func TestRetentionRecordsAreForwardOnlyAndHoldNoPersonalData(t *testing.T) {
	pool := postgresPool(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	for table, want := range retentionRecordColumns {
		var got string
		if err := pool.QueryRow(ctx, `SELECT string_agg(column_name, ',' ORDER BY column_name) FROM information_schema.columns
			WHERE table_schema = 'public' AND table_name = $1`, table).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("%s columns %s, want %s", table, got, want)
		}
	}
	// Nothing deletes a record by cascade from another table.
	var cascading int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM pg_constraint
		WHERE contype = 'f' AND confdeltype IN ('c', 'n', 'd')
		  AND conrelid IN ('retention_periods'::regclass, 'retention_runs'::regclass, 'retention_run_rules'::regclass)`).Scan(&cascading); err != nil || cascading != 0 {
		t.Fatalf("cascading foreign keys into the records=%d err=%v", cascading, err)
	}

	// Free text cannot get in: rule, table and error code are identifiers.
	runID := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO retention_runs (id, mode, triggered_by, full_run, rule_set_version, started_at, status)
		VALUES ($1, 'dry-run', 'cli', true, 1, now(), 'running')`, runID); err != nil {
		t.Fatal(err)
	}
	for name, insert := range map[string]string{
		"an address as error code": `INSERT INTO retention_run_rules (run_id, rule, rule_version, kind, action, target_table, status, error_code, started_at, finished_at)
			VALUES ($1, 'guest_phone', 1, 'sweep', 'scrub', 'tickets', 'failed', 'ada@example.com', now(), now())`,
		"a sentence as rule": `INSERT INTO retention_run_rules (run_id, rule, rule_version, kind, action, target_table, status, started_at, finished_at)
			VALUES ($1, 'Ada Lovelace', 1, 'sweep', 'scrub', 'tickets', 'ok', now(), now())`,
		"an unknown status": `INSERT INTO retention_run_rules (run_id, rule, rule_version, kind, action, target_table, status, started_at, finished_at)
			VALUES ($1, 'guest_phone', 1, 'sweep', 'scrub', 'tickets', 'deleted ada', now(), now())`,
		"a negative count": `INSERT INTO retention_run_rules (run_id, rule, rule_version, kind, action, target_table, status, matched, started_at, finished_at)
			VALUES ($1, 'guest_phone', 1, 'sweep', 'scrub', 'tickets', 'ok', -1, now(), now())`,
	} {
		if _, err := pool.Exec(ctx, insert, runID); err == nil || !strings.Contains(err.Error(), "23514") {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO retention_periods (id, started_at, ends_at) VALUES ($1, now(), now() + interval '90 days'), ($2, now(), now() + interval '90 days')`,
		uuid.New(), uuid.New()); err == nil || !strings.Contains(err.Error(), "23505") {
		t.Fatalf("two open periods: %v", err)
	}

	down, err := fs.ReadFile(db.DownSQL, "migrations/"+retentionRunsVersion+"_retention_runs.down.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, string(down)); err == nil || !strings.Contains(err.Error(), "forward-only") {
		t.Fatalf("down with a record: %v", err)
	}
	var runs int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM retention_runs`).Scan(&runs); err != nil || runs != 1 {
		t.Fatalf("records after a refused down: %d %v", runs, err)
	}

	// Without a record the down is clean, and Apply puts the tables back.
	if _, err := pool.Exec(ctx, `DELETE FROM retention_runs`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, string(down)); err != nil {
		t.Fatalf("down without records: %v", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM schema_migrations WHERE version = `+retentionRunsVersion); err != nil {
		t.Fatal(err)
	}
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	var back bool
	if err := pool.QueryRow(ctx, `SELECT to_regclass('public.retention_run_rules') IS NOT NULL
		AND EXISTS (SELECT 1 FROM schema_migrations WHERE version = `+retentionRunsVersion+`)`).Scan(&back); err != nil || !back {
		t.Fatalf("tables back %v %v", back, err)
	}
	// A database that has the tables but lost the record of the migration
	// is recorded without running it again.
	if _, err := pool.Exec(ctx, `DELETE FROM schema_migrations WHERE version = `+retentionRunsVersion); err != nil {
		t.Fatal(err)
	}
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
}
