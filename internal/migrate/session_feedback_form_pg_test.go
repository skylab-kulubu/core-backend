package migrate_test

import (
	"context"
	"slices"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
)

const sessionFeedbackFormVersion = "20261020190100"

func TestSessionFeedbackFormMigrationComesLast(t *testing.T) {
	t.Parallel()
	versions, err := migrate.Versions()
	if err != nil {
		t.Fatal(err)
	}
	before := slices.Index(versions, 20261020190000)
	session := slices.Index(versions, 20261020190100)
	if before < 0 || session < 0 || before >= session {
		t.Fatalf("versions %v", versions)
	}
}

// A Session written before the migration gets no form and is mailed
// automatically; the release before keeps writing; down and up again work,
// and the fingerprint records a schema that already has the columns.
func TestSessionFeedbackFormMigration(t *testing.T) {
	pool := postgresPool(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, readDown(t, sessionFeedbackFormVersion+"_session_feedback_form.down.sql")); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM schema_migrations WHERE version = `+sessionFeedbackFormVersion); err != nil {
		t.Fatal(err)
	}
	eventID, dayID, sessionID := uuid.New(), uuid.New(), uuid.New()
	oldRelease := func(id uuid.UUID) {
		t.Helper()
		if _, err := pool.Exec(ctx, `INSERT INTO sessions (id, event_day_id, title, speaker_name, session_type) VALUES ($1, $2, 'Açılış', 'Ada', 'talk')`, id, dayID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO events (id, name, location, owner_team) VALUES ($1, 'Hack', 'YTÜ', 'WEBLAB')`, eventID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO event_days (id, event_id) VALUES ($1, $2)`, dayID, eventID); err != nil {
		t.Fatal(err)
	}
	oldRelease(sessionID)
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	var url, alias string
	var autoSend bool
	if err := pool.QueryRow(ctx, `SELECT feedback_form_url, feedback_form_alias, feedback_auto_send FROM sessions WHERE id = $1`, sessionID).
		Scan(&url, &alias, &autoSend); err != nil {
		t.Fatal(err)
	}
	if url != "" || alias != "" || !autoSend {
		t.Fatalf("existing Session: %q %q %v", url, alias, autoSend)
	}
	oldRelease(uuid.New())
	if _, err := pool.Exec(ctx, `DELETE FROM schema_migrations WHERE version = `+sessionFeedbackFormVersion); err != nil {
		t.Fatal(err)
	}
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if !recorded(t, pool, sessionFeedbackFormVersion) {
		t.Fatal("not recorded by its fingerprint")
	}
}
