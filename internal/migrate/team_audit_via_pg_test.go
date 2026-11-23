package migrate_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
)

const (
	teamAuditViaVersion = "20261123200000"
	teamAuditViaDown    = teamAuditViaVersion + "_team_audit_via.down.sql"
)

// The way column: the rows already there, and the rows the release before
// writes (no via in its insert), have none; the up migration runs again
// over itself (its fingerprint records it); down drops the column and its
// check and leaves the rows, and up then adds it back.
func TestTeamAuditViaMigration(t *testing.T) {
	pool := postgresPool(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, readDown(t, teamAuditViaDown)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM schema_migrations WHERE version = `+teamAuditViaVersion); err != nil {
		t.Fatal(err)
	}
	// The insert of the release before: no via.
	oldInsert := `INSERT INTO team_audit_records (id, occurred_at, actor_id, actor_basis, action, group_id, group_path, target_user_id, outcome)
		VALUES ($1, now(), gen_random_uuid(), 'privileged', 'member.add', gen_random_uuid(), '/UYELER/ARGE', gen_random_uuid(), 'ok')`
	before := uuid.New()
	if _, err := pool.Exec(ctx, oldInsert, before); err != nil {
		t.Fatal(err)
	}

	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	after := uuid.New()
	if _, err := pool.Exec(ctx, oldInsert, after); err != nil {
		t.Fatalf("the release before writes on the new schema: %v", err)
	}
	var nulls int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM team_audit_records WHERE id = ANY($1) AND via IS NULL`,
		[]uuid.UUID{before, after}).Scan(&nulls); err != nil {
		t.Fatal(err)
	}
	if nulls != 2 {
		t.Fatalf("%d of 2 rows without a way", nulls)
	}

	// Again over itself: recorded, nothing changes.
	if _, err := pool.Exec(ctx, `DELETE FROM schema_migrations WHERE version = `+teamAuditViaVersion); err != nil {
		t.Fatal(err)
	}
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if !recorded(t, pool, teamAuditViaVersion) {
		t.Fatal("not recorded")
	}

	if _, err := pool.Exec(ctx, `UPDATE team_audit_records SET via = 'invitation' WHERE id = $1`, after); err == nil {
		t.Fatal("a finished row took a way")
	}
	if _, err := pool.Exec(ctx, readDown(t, teamAuditViaDown)); err != nil {
		t.Fatal(err)
	}
	var columns, rows int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM information_schema.columns
		WHERE table_schema = 'public' AND table_name = 'team_audit_records' AND column_name = 'via'`).Scan(&columns); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM team_audit_records WHERE id = ANY($1)`, []uuid.UUID{before, after}).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if columns != 0 || rows != 2 {
		t.Fatalf("after down: %d via columns, %d rows", columns, rows)
	}
	// The finish-only trigger works without the column.
	if _, err := pool.Exec(ctx, `UPDATE team_audit_records SET group_path = '/OTHER' WHERE id = $1`, after); err == nil {
		t.Fatal("after down a finished row changed")
	}
	if _, err := pool.Exec(ctx, `DELETE FROM schema_migrations WHERE version = `+teamAuditViaVersion); err != nil {
		t.Fatal(err)
	}
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO team_audit_records (id, occurred_at, actor_id, actor_basis, action, group_id, group_path, target_user_id, outcome, via)
		VALUES (gen_random_uuid(), now(), gen_random_uuid(), 'privileged', 'member.add', gen_random_uuid(), '/UYELER/ARGE', gen_random_uuid(), 'ok', 'access_review')`); err != nil {
		t.Fatalf("up again: %v", err)
	}
}
