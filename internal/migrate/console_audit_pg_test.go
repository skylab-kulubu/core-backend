package migrate_test

import (
	"context"
	"strings"
	"testing"

	"github.com/skylab-kulubu/core-backend/internal/migrate"
)

const (
	consoleAuditVersion = "20261126110000"
	consoleAuditDown    = consoleAuditVersion + "_console_audit.down.sql"
)

// The console audit (docs/console-audit.md): up takes a console row once
// per event; down deletes the console rows, the cursor and the notice
// marker, and leaves the basis check without console but with the term
// job's member.remove (20261126101700); up again restores it all.
func TestConsoleAuditMigration(t *testing.T) {
	pool := postgresPool(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	insert := `INSERT INTO team_audit_records (id, occurred_at, actor_id, actor_basis, action, group_path, outcome, via, keycloak_event_id, client)
		VALUES (gen_random_uuid(), now(), gen_random_uuid(), 'console', 'keycloak.client', '', 'ok', 'console', $1, 'keycloak:realm')`
	if _, err := pool.Exec(ctx, insert, "ev-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, insert, "ev-1"); err == nil {
		t.Fatal("the same event twice")
	}
	if _, err := pool.Exec(ctx, `INSERT INTO console_audit_cursor (event_time) VALUES (1)`); err != nil {
		t.Fatal(err)
	}

	if _, err := pool.Exec(ctx, readDown(t, consoleAuditDown)); err != nil {
		t.Fatalf("down: %v", err)
	}
	var consoleRows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM team_audit_records WHERE via = 'console'`).Scan(&consoleRows); err != nil || consoleRows != 0 {
		t.Fatalf("console rows after down: %d %v", consoleRows, err)
	}
	var cursor, column, marker bool
	if err := pool.QueryRow(ctx, `SELECT
		to_regclass('public.console_audit_cursor') IS NOT NULL,
		EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = 'team_audit_records' AND column_name = 'keycloak_event_id'),
		EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = 'privilege_notice_mails' AND column_name = 'via')`).Scan(&cursor, &column, &marker); err != nil {
		t.Fatal(err)
	}
	if cursor || column || marker {
		t.Fatalf("after down: cursor %v, event id %v, marker %v", cursor, column, marker)
	}
	var basis string
	if err := pool.QueryRow(ctx, `SELECT pg_get_constraintdef(oid) FROM pg_constraint
		WHERE conrelid = to_regclass('public.team_audit_records') AND conname = 'team_audit_records_actor_basis_check'`).Scan(&basis); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(basis, "console") || !strings.Contains(basis, "member.remove") {
		t.Fatalf("basis check after down: %s", basis)
	}

	if _, err := pool.Exec(ctx, `DELETE FROM schema_migrations WHERE version = `+consoleAuditVersion); err != nil {
		t.Fatal(err)
	}
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatalf("up again: %v", err)
	}
	if _, err := pool.Exec(ctx, insert, "ev-2"); err != nil {
		t.Fatalf("a console row after up again: %v", err)
	}
}
