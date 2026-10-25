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
	dropEventsActiveVersion = "20261025200000"
	dropEventsActiveDown    = dropEventsActiveVersion + "_drop_events_active.down.sql"
)

func eventsHasActive(t *testing.T, pool *pgxpool.Pool) bool {
	t.Helper()
	var has bool
	if err := pool.QueryRow(context.Background(), `
		SELECT EXISTS (SELECT 1 FROM information_schema.columns
		WHERE table_schema = 'public' AND table_name = 'events' AND column_name = 'active')`).Scan(&has); err != nil {
		t.Fatal(err)
	}
	return has
}

// The drop is numbered after every migration before it, so on production's
// database it runs last.
func TestDropEventsActiveMigrationComesLast(t *testing.T) {
	t.Parallel()
	versions, err := migrate.Versions()
	if err != nil {
		t.Fatal(err)
	}
	// The newest migration on main when this one was written.
	before := slices.Index(versions, 20261025180100)
	this := slices.Index(versions, 20261025200000)
	if before < 0 || this < 0 || before >= this {
		t.Fatalf("versions %v", versions)
	}
}

// Apply drops events.active (ADR 0066: active is derived); the down
// migration puts it back as the derived active of each Event, for a release
// that reads it; Apply drops it again; a database that already lost it is
// recorded.
func TestDropEventsActiveMigration(t *testing.T) {
	pool := postgresPool(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if eventsHasActive(t, pool) {
		t.Fatal("events.active still there")
	}
	past := time.Now().Add(-72 * time.Hour)
	future := time.Now().Add(72 * time.Hour)
	want := map[uuid.UUID]bool{}
	for _, row := range []struct {
		status string
		start  *time.Time
		active bool
	}{
		{"published", &future, true},
		{"published", nil, true},
		{"published", &past, false},
		{"draft", &future, false},
		{"cancelled", &future, false},
	} {
		id := uuid.New()
		if _, err := pool.Exec(ctx, `INSERT INTO events (id, name, location, owner_team, start_date, status) VALUES ($1, 'Etkinlik', 'YTÜ', 'WEBLAB', $2, $3)`,
			id, row.start, row.status); err != nil {
			t.Fatal(err)
		}
		want[id] = row.active
	}

	if _, err := pool.Exec(ctx, readDown(t, dropEventsActiveDown)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM schema_migrations WHERE version = `+dropEventsActiveVersion); err != nil {
		t.Fatal(err)
	}
	for id, active := range want {
		var got bool
		if err := pool.QueryRow(ctx, `SELECT active FROM events WHERE id = $1`, id).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != active {
			t.Errorf("after down %s: active=%v, want %v", id, got, active)
		}
	}

	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if eventsHasActive(t, pool) || !recorded(t, pool, dropEventsActiveVersion) {
		t.Fatal("not dropped again")
	}

	// Already gone, not recorded: the fingerprint records it.
	if _, err := pool.Exec(ctx, `DELETE FROM schema_migrations WHERE version = `+dropEventsActiveVersion); err != nil {
		t.Fatal(err)
	}
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if !recorded(t, pool, dropEventsActiveVersion) {
		t.Fatal("not recorded")
	}
}
