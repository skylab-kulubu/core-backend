package migrate_test

import (
	"context"
	"testing"

	"github.com/skylab-kulubu/core-backend/internal/migrate"
)

const (
	doorWriteRecordVersion = "20261128120000"
	doorWriteRecordDown    = doorWriteRecordVersion + "_door_write_record.down.sql"
)

// Who wrote a Check-in and the door's taken-back marks: the up migration
// runs again over itself (its fingerprint records it); down drops the two
// columns and door_undos, and up adds them back.
func TestDoorWriteRecordMigration(t *testing.T) {
	pool := postgresPool(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	present := func() (int, bool) {
		t.Helper()
		var columns int
		var table bool
		if err := pool.QueryRow(ctx, `
			SELECT (SELECT count(*) FROM information_schema.columns
				WHERE table_schema = 'public' AND table_name = 'ticket_checkins' AND column_name IN ('source', 'by_user_id')),
				to_regclass('public.door_undos') IS NOT NULL`).Scan(&columns, &table); err != nil {
			t.Fatal(err)
		}
		return columns, table
	}
	if columns, table := present(); columns != 2 || !table {
		t.Fatalf("%d columns, door_undos %v", columns, table)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM schema_migrations WHERE version = `+doorWriteRecordVersion); err != nil {
		t.Fatal(err)
	}
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if !recorded(t, pool, doorWriteRecordVersion) {
		t.Fatal("not recorded")
	}
	if _, err := pool.Exec(ctx, readDown(t, doorWriteRecordDown)); err != nil {
		t.Fatal(err)
	}
	if columns, table := present(); columns != 0 || table {
		t.Fatalf("after down: %d columns, door_undos %v", columns, table)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM schema_migrations WHERE version = `+doorWriteRecordVersion); err != nil {
		t.Fatal(err)
	}
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if columns, table := present(); columns != 2 || !table {
		t.Fatalf("up again: %d columns, door_undos %v", columns, table)
	}
}
