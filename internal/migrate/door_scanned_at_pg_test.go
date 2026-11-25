package migrate_test

import (
	"context"
	"testing"

	"github.com/skylab-kulubu/core-backend/internal/migrate"
)

const (
	doorScannedAtVersion = "20261126102700"
	doorScannedAtDown    = doorScannedAtVersion + "_door_scanned_at.down.sql"
)

// The offline door's column: the up migration runs again over itself (its
// fingerprint records it); down drops both columns, and up adds them back.
func TestDoorScannedAtMigration(t *testing.T) {
	pool := postgresPool(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	columns := func() int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, `
			SELECT count(*) FROM information_schema.columns
			WHERE table_schema = 'public' AND column_name = 'scanned_at' AND table_name IN ('ticket_checkins', 'venue_entries')`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if got := columns(); got != 2 {
		t.Fatalf("%d columns", got)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM schema_migrations WHERE version = `+doorScannedAtVersion); err != nil {
		t.Fatal(err)
	}
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if !recorded(t, pool, doorScannedAtVersion) {
		t.Fatal("not recorded")
	}
	if _, err := pool.Exec(ctx, readDown(t, doorScannedAtDown)); err != nil {
		t.Fatal(err)
	}
	if got := columns(); got != 0 {
		t.Fatalf("after down: %d columns", got)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM schema_migrations WHERE version = `+doorScannedAtVersion); err != nil {
		t.Fatal(err)
	}
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if got := columns(); got != 2 {
		t.Fatalf("up again: %d columns", got)
	}
}
