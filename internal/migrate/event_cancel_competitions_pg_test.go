package migrate_test

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
)

const (
	eventCancelCompetitionsVersion = "20261020130000"
	eventCancelCompetitionsDown    = eventCancelCompetitionsVersion + "_event_cancel_competitions.down.sql"
)

// The migration comes after every migration on main when it was written.
func TestEventCancelCompetitionsMigrationComesLast(t *testing.T) {
	t.Parallel()
	versions, err := migrate.Versions()
	if err != nil {
		t.Fatal(err)
	}
	before := slices.Index(versions, 20261019120000)
	this := slices.Index(versions, 20261020130000)
	if before < 0 || this < 0 || before >= this {
		t.Fatalf("versions %v", versions)
	}
}

// The check keeps the two columns together and the earlier status one a
// Competition can come back to; down and up again keep working, and the
// fingerprint records a schema that has them.
func TestEventCancelCompetitionsMigration(t *testing.T) {
	pool := postgresPool(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	eventID := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO events (id, name, description, location, owner_team, form_url, capacity, linkedin, active, ranked, prize_info, attendance_rule, extra_form_urls)
		VALUES ($1, 'Etkinlik', '', 'YTÜ', 'WEBLAB', '', 0, '', true, false, '', 'none', '{}')`, eventID); err != nil {
		t.Fatal(err)
	}
	insert := func(status, reason, before string, withEvent bool) error {
		var b any
		if before != "" {
			b = before
		}
		at := "NULL"
		if withEvent {
			at = "now()"
		}
		_, err := pool.Exec(ctx, `INSERT INTO competitions (id, event_id, name, status, cancel_reason, status_before_event_cancel, cancelled_with_event_at)
			VALUES ($1, $2, 'yarışma', $3, $4, $5, `+at+`)`, uuid.New(), eventID, status, reason, b)
		return err
	}
	if err := insert("cancelled", "Etkinlik iptal edildi.", "running", true); err != nil {
		t.Fatal(err)
	}
	if err := insert("open", "", "", false); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []struct {
		before    string
		withEvent bool
	}{{"running", false}, {"", true}, {"cancelled", true}} {
		if err := insert("cancelled", "x", bad.before, bad.withEvent); err == nil || !strings.Contains(err.Error(), "competitions_status_before_event_cancel_check") {
			t.Fatalf("%+v: %v", bad, err)
		}
	}

	if _, err := pool.Exec(ctx, readDown(t, eventCancelCompetitionsDown)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM schema_migrations WHERE version = `+eventCancelCompetitionsVersion); err != nil {
		t.Fatal(err)
	}
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if !recorded(t, pool, eventCancelCompetitionsVersion) {
		t.Fatal("not recorded")
	}
	if _, err := pool.Exec(ctx, `DELETE FROM schema_migrations WHERE version = `+eventCancelCompetitionsVersion); err != nil {
		t.Fatal(err)
	}
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if !recorded(t, pool, eventCancelCompetitionsVersion) {
		t.Fatal("not recorded by its fingerprint")
	}
}
