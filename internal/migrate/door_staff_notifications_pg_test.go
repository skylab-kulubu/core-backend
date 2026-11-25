package migrate_test

import (
	"context"
	"testing"

	"github.com/skylab-kulubu/core-backend/internal/migrate"
)

const (
	doorStaffMailVersion = "20261126150000"
	doorStaffMailDown    = doorStaffMailVersion + "_door_staff_notifications.down.sql"
)

// The door staff mail's queue: the up migration runs again over itself (its
// fingerprint records it); down drops it, and up adds it back.
func TestDoorStaffNotificationsMigration(t *testing.T) {
	pool := postgresPool(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	present := func() bool {
		t.Helper()
		var ok bool
		if err := pool.QueryRow(ctx, `SELECT to_regclass('public.door_staff_notifications') IS NOT NULL`).Scan(&ok); err != nil {
			t.Fatal(err)
		}
		return ok
	}
	if !present() {
		t.Fatal("no table")
	}
	if _, err := pool.Exec(ctx, `DELETE FROM schema_migrations WHERE version = `+doorStaffMailVersion); err != nil {
		t.Fatal(err)
	}
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if !recorded(t, pool, doorStaffMailVersion) {
		t.Fatal("not recorded")
	}
	if _, err := pool.Exec(ctx, readDown(t, doorStaffMailDown)); err != nil {
		t.Fatal(err)
	}
	if present() {
		t.Fatal("after down: still there")
	}
	if _, err := pool.Exec(ctx, `DELETE FROM schema_migrations WHERE version = `+doorStaffMailVersion); err != nil {
		t.Fatal(err)
	}
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if !present() {
		t.Fatal("up again: no table")
	}
}
