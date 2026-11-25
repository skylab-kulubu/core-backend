package migrate_test

import (
	"context"
	"testing"

	"github.com/skylab-kulubu/core-backend/internal/migrate"
)

const (
	eventChangeNoticeVersion = "20261126140000"
	eventChangeNoticeDown    = eventChangeNoticeVersion + "_event_change_notice_choices.down.sql"
)

// The editor's choice: the up migration runs again over itself (its
// fingerprint records it); down drops the record and the told fields, and
// up adds them back. decided_by empties when its account is deleted.
func TestEventChangeNoticeChoicesMigration(t *testing.T) {
	pool := postgresPool(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	present := func() int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, `
			SELECT (SELECT count(*) FROM information_schema.columns
				WHERE table_schema = 'public' AND table_name = 'event_update_notifications' AND column_name = 'told_fields')
			+ CASE WHEN to_regclass('public.event_change_notice_choices') IS NULL THEN 0 ELSE 1 END`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if got := present(); got != 2 {
		t.Fatalf("%d of 2 objects", got)
	}
	var deleteAction string
	if err := pool.QueryRow(ctx, `
		SELECT confdeltype::text FROM pg_constraint
		WHERE conrelid = 'event_change_notice_choices'::regclass AND contype = 'f' AND confrelid = 'users'::regclass`).Scan(&deleteAction); err != nil || deleteAction != "n" {
		t.Fatalf("decided_by on delete %q %v, want SET NULL", deleteAction, err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM schema_migrations WHERE version = `+eventChangeNoticeVersion); err != nil {
		t.Fatal(err)
	}
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if !recorded(t, pool, eventChangeNoticeVersion) {
		t.Fatal("not recorded")
	}
	if _, err := pool.Exec(ctx, readDown(t, eventChangeNoticeDown)); err != nil {
		t.Fatal(err)
	}
	if got := present(); got != 0 {
		t.Fatalf("after down: %d objects", got)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM schema_migrations WHERE version = `+eventChangeNoticeVersion); err != nil {
		t.Fatal(err)
	}
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if got := present(); got != 2 {
		t.Fatalf("up again: %d objects", got)
	}
}
