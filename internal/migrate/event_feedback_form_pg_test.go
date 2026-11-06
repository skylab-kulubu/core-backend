package migrate_test

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
)

const eventFeedbackFormVersion = "20261020190000"

// The feedback form migration comes after every migration on main when it
// was written.
func TestEventFeedbackFormMigrationComesLast(t *testing.T) {
	t.Parallel()
	versions, err := migrate.Versions()
	if err != nil {
		t.Fatal(err)
	}
	before := slices.Index(versions, 20261020180000)
	feedback := slices.Index(versions, 20261020190000)
	if before < 0 || feedback < 0 || before >= feedback {
		t.Fatalf("versions %v", versions)
	}
}

// An Event written before the migration gets the defaults (mode event, no
// form, sent automatically); the release before keeps writing on the new
// schema; a mode outside the three is refused; down and up again work, and
// the fingerprint records a schema that already has the columns.
func TestEventFeedbackFormMigration(t *testing.T) {
	pool := postgresPool(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	// The release before writes the legacy active column: put it back; the
	// drop stays recorded, so the Apply calls below leave it.
	if _, err := pool.Exec(ctx, readDown(t, dropEventsActiveDown)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, readDown(t, eventFeedbackFormVersion+"_event_feedback_form.down.sql")); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM schema_migrations WHERE version = `+eventFeedbackFormVersion); err != nil {
		t.Fatal(err)
	}
	existing := uuid.New()
	insertAsTheOldRelease(t, pool, existing, true, time.Date(2026, 9, 19, 10, 0, 0, 0, time.UTC))
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	var mode, url, alias string
	var autoSend bool
	if err := pool.QueryRow(ctx, `SELECT feedback_mode, feedback_form_url, feedback_form_alias, feedback_auto_send FROM events WHERE id = $1`, existing).
		Scan(&mode, &url, &alias, &autoSend); err != nil {
		t.Fatal(err)
	}
	if mode != "event" || url != "" || alias != "" || !autoSend {
		t.Fatalf("existing Event: %q %q %q %v", mode, url, alias, autoSend)
	}
	insertAsTheOldRelease(t, pool, uuid.New(), true, time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC))
	if _, err := pool.Exec(ctx, `UPDATE events SET feedback_mode = 'everyone' WHERE id = $1`, existing); err == nil {
		t.Fatal("unknown mode taken")
	}
	if _, err := pool.Exec(ctx, `DELETE FROM schema_migrations WHERE version = `+eventFeedbackFormVersion); err != nil {
		t.Fatal(err)
	}
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if !recorded(t, pool, eventFeedbackFormVersion) {
		t.Fatal("not recorded by its fingerprint")
	}
}
