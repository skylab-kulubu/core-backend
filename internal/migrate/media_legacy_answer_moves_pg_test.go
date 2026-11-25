package migrate_test

import (
	"context"
	"testing"

	"github.com/skylab-kulubu/core-backend/internal/migrate"
)

const (
	legacyAnswerMovesVersion = "20261126180000"
	legacyAnswerMovesDown    = legacyAnswerMovesVersion + "_media_legacy_answer_moves.down.sql"
)

// The legacy Answer file moves' record: the up migration runs again over
// itself (its fingerprint records it); down drops the table, and up makes it
// again.
func TestMediaLegacyAnswerMovesMigration(t *testing.T) {
	pool := postgresPool(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	exists := func() bool {
		t.Helper()
		var ok bool
		if err := pool.QueryRow(ctx, `SELECT to_regclass('public.media_legacy_answer_moves') IS NOT NULL`).Scan(&ok); err != nil {
			t.Fatal(err)
		}
		return ok
	}
	if !exists() {
		t.Fatal("no media_legacy_answer_moves")
	}
	if _, err := pool.Exec(ctx, `DELETE FROM schema_migrations WHERE version = `+legacyAnswerMovesVersion); err != nil {
		t.Fatal(err)
	}
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if !recorded(t, pool, legacyAnswerMovesVersion) {
		t.Fatal("not recorded")
	}
	if _, err := pool.Exec(ctx, readDown(t, legacyAnswerMovesDown)); err != nil {
		t.Fatal(err)
	}
	if exists() {
		t.Fatal("after down: the table is still there")
	}
	if _, err := pool.Exec(ctx, `DELETE FROM schema_migrations WHERE version = `+legacyAnswerMovesVersion); err != nil {
		t.Fatal(err)
	}
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if !exists() {
		t.Fatal("up again: no table")
	}
}
