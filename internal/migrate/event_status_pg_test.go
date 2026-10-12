package migrate_test

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
)

const (
	eventStatusVersion = "20261012130000"
	eventStatusDown    = eventStatusVersion + "_event_status.down.sql"
)

var eventStatusColumns = []string{"status", "published_at", "published_by", "cancelled_at", "cancelled_by", "cancel_reason", "copied_from_event_id"}

func eventStatusColumnCount(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `
		SELECT count(*) FROM information_schema.columns
		WHERE table_schema = 'public' AND table_name = 'events' AND column_name = ANY($1)`, eventStatusColumns).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// The release before the status, as it writes an Event: it names its
// columns and knows nothing of the status.
func insertAsTheOldRelease(t *testing.T, pool *pgxpool.Pool, id uuid.UUID, active bool, createdAt time.Time) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO events (
			id, name, description, location, owner_team, form_url, capacity,
			start_date, end_date, linkedin, active, ranked, prize_info, season_id, cover_image_id,
			attendance_rule, attendance_ratio, extra_form_urls, created_at
		) VALUES ($1,'Etkinlik','','YTÜ','WEBLAB','',0,NULL,NULL,'',$2,false,'',NULL,NULL,'none',NULL,'{}',$3)`,
		id, active, createdAt); err != nil {
		t.Fatal(err)
	}
}

// The status migration is numbered after every migration before it, so on
// production's database it runs last.
func TestEventStatusMigrationComesLast(t *testing.T) {
	t.Parallel()
	versions, err := migrate.Versions()
	if err != nil {
		t.Fatal(err)
	}
	// The newest migration on main when this one was written.
	before := slices.Index(versions, 20261012120000)
	status := slices.Index(versions, 20261012130000)
	if before < 0 || status < 0 || before >= status {
		t.Fatalf("versions %v", versions)
	}
}

// On a database with Events (production's), every Event becomes published
// as of its creation and keeps its legacy active flag untouched; the release
// before keeps writing on the new schema; the down migration takes the
// status away and leaves the flag as it was; Apply brings it back.
func TestEventStatusMigrationOnExistingEvents(t *testing.T) {
	pool := postgresPool(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if n := eventStatusColumnCount(t, pool); n != len(eventStatusColumns) {
		t.Fatalf("fresh database: %d status columns", n)
	}
	// Back to the schema before the status.
	if _, err := pool.Exec(ctx, readDown(t, eventStatusDown)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM schema_migrations WHERE version = `+eventStatusVersion); err != nil {
		t.Fatal(err)
	}
	if n := eventStatusColumnCount(t, pool); n != 0 {
		t.Fatalf("after down: %d status columns", n)
	}
	shown, hidden := uuid.New(), uuid.New()
	shownAt := time.Date(2026, 9, 19, 10, 0, 0, 0, time.UTC)
	hiddenAt := time.Date(2026, 9, 26, 10, 0, 0, 0, time.UTC)
	insertAsTheOldRelease(t, pool, shown, true, shownAt)
	insertAsTheOldRelease(t, pool, hidden, false, hiddenAt)

	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if !recorded(t, pool, eventStatusVersion) {
		t.Fatal("not recorded")
	}
	for id, want := range map[uuid.UUID]struct {
		active bool
		at     time.Time
	}{shown: {true, shownAt}, hidden: {false, hiddenAt}} {
		var status, reason string
		var active bool
		var publishedAt *time.Time
		var publishedBy, cancelledBy, copiedFrom *uuid.UUID
		var cancelledAt *time.Time
		if err := pool.QueryRow(ctx, `
			SELECT status, active, published_at, published_by, cancelled_at, cancelled_by, cancel_reason, copied_from_event_id
			FROM events WHERE id = $1`, id).Scan(&status, &active, &publishedAt, &publishedBy, &cancelledAt, &cancelledBy, &reason, &copiedFrom); err != nil {
			t.Fatal(err)
		}
		if status != "published" || active != want.active || publishedAt == nil || !publishedAt.Equal(want.at) ||
			publishedBy != nil || cancelledAt != nil || cancelledBy != nil || reason != "" || copiedFrom != nil {
			t.Fatalf("event %s: status=%q active=%v publishedAt=%v by=%v cancelled=%v/%v %q copied=%v",
				id, status, active, publishedAt, publishedBy, cancelledAt, cancelledBy, reason, copiedFrom)
		}
	}

	// The check takes the three statuses and nothing else.
	for _, status := range []string{"draft", "published", "cancelled"} {
		if _, err := pool.Exec(ctx, `UPDATE events SET status = $2 WHERE id = $1`, hidden, status); err != nil {
			t.Fatalf("status %q: %v", status, err)
		}
	}
	if _, err := pool.Exec(ctx, `UPDATE events SET status = 'live' WHERE id = $1`, hidden); err == nil {
		t.Fatal("an unknown status was stored")
	}
	var indexed bool
	if err := pool.QueryRow(ctx, `SELECT to_regclass('public.events_current_status_idx') IS NOT NULL`).Scan(&indexed); err != nil || !indexed {
		t.Fatalf("status index %v %v", indexed, err)
	}

	// The release before writes on the new schema as it did: a new Event
	// takes the defaults, an update of the flag touches nothing else.
	old := uuid.New()
	insertAsTheOldRelease(t, pool, old, false, time.Now())
	if _, err := pool.Exec(ctx, `UPDATE events SET active = true, updated_at = now() WHERE id = $1`, old); err != nil {
		t.Fatal(err)
	}
	var status string
	var active bool
	if err := pool.QueryRow(ctx, `SELECT status, active FROM events WHERE id = $1`, old).Scan(&status, &active); err != nil || status != "published" || !active {
		t.Fatalf("old release's event status=%q active=%v %v", status, active, err)
	}

	// Rolled back: the status goes, the flag stays as it was.
	if _, err := pool.Exec(ctx, readDown(t, eventStatusDown)); err != nil {
		t.Fatal(err)
	}
	if n := eventStatusColumnCount(t, pool); n != 0 {
		t.Fatalf("after down: %d status columns", n)
	}
	var shownActive, hiddenActive bool
	if err := pool.QueryRow(ctx, `SELECT (SELECT active FROM events WHERE id = $1), (SELECT active FROM events WHERE id = $2)`, shown, hidden).Scan(&shownActive, &hiddenActive); err != nil {
		t.Fatal(err)
	}
	if !shownActive || hiddenActive {
		t.Fatalf("after down active shown=%v hidden=%v", shownActive, hiddenActive)
	}
	var triggers int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_trigger WHERE tgrelid = 'events'::regclass AND tgname IN ('events_require_active_publisher', 'events_require_active_canceller')`).Scan(&triggers); err != nil || triggers != 0 {
		t.Fatalf("triggers after down %d %v", triggers, err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM schema_migrations WHERE version = `+eventStatusVersion); err != nil {
		t.Fatal(err)
	}
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if n := eventStatusColumnCount(t, pool); n != len(eventStatusColumns) {
		t.Fatalf("after a second up: %d status columns", n)
	}
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
}

// On an empty database the migration goes down and up again cleanly, and the
// fingerprint records it when its schema is already there.
func TestEventStatusMigrationOnAnEmptyDatabase(t *testing.T) {
	pool := postgresPool(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, readDown(t, eventStatusDown)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, readDown(t, eventStatusDown)); err != nil {
		t.Fatalf("a second down: %v", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM schema_migrations WHERE version = `+eventStatusVersion); err != nil {
		t.Fatal(err)
	}
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if n := eventStatusColumnCount(t, pool); n != len(eventStatusColumns) {
		t.Fatalf("%d status columns", n)
	}
	// The schema is there but not recorded: Apply records it.
	if _, err := pool.Exec(ctx, `DELETE FROM schema_migrations WHERE version = `+eventStatusVersion); err != nil {
		t.Fatal(err)
	}
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if !recorded(t, pool, eventStatusVersion) {
		t.Fatal("not recorded")
	}
}
