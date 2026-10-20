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
	registrationClosureVersion = "20261020120000"
	registrationClosureDown    = registrationClosureVersion + "_event_registration_closure.down.sql"
)

func registrationClosureColumns(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `
		SELECT count(*) FROM information_schema.columns
		WHERE table_schema = 'public' AND table_name = 'events'
		  AND column_name IN ('registration_closed', 'registration_closes_at', 'registration_closed_at')`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// The registration closure migration comes after every migration on main
// when it was written.
func TestRegistrationClosureMigrationComesLast(t *testing.T) {
	t.Parallel()
	versions, err := migrate.Versions()
	if err != nil {
		t.Fatal(err)
	}
	before := slices.Index(versions, 20261019120000)
	closure := slices.Index(versions, 20261020120000)
	if before < 0 || closure < 0 || before >= closure {
		t.Fatalf("versions %v", versions)
	}
}

// An Event written before the migration (by the release before it) is open;
// that release keeps writing on the new schema; down and up again keep
// working, and the fingerprint records a schema that already has it.
func TestRegistrationClosureMigrationOnExistingEvents(t *testing.T) {
	pool := postgresPool(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if n := registrationClosureColumns(t, pool); n != 3 {
		t.Fatalf("fresh database: %d columns", n)
	}
	if _, err := pool.Exec(ctx, readDown(t, registrationClosureDown)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM schema_migrations WHERE version = `+registrationClosureVersion); err != nil {
		t.Fatal(err)
	}
	if n := registrationClosureColumns(t, pool); n != 0 {
		t.Fatalf("after down: %d columns", n)
	}
	before := uuid.New()
	insertAsTheOldRelease(t, pool, before, true, time.Date(2026, 9, 19, 10, 0, 0, 0, time.UTC))

	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if !recorded(t, pool, registrationClosureVersion) {
		t.Fatal("not recorded")
	}
	var closed bool
	var closesAt *time.Time
	if err := pool.QueryRow(ctx, `SELECT registration_closed, registration_closes_at FROM events WHERE id = $1`, before).Scan(&closed, &closesAt); err != nil {
		t.Fatal(err)
	}
	if closed || closesAt != nil {
		t.Fatalf("existing Event: closed=%v closesAt=%v", closed, closesAt)
	}
	// The release before writes on the new schema.
	insertAsTheOldRelease(t, pool, uuid.New(), true, time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC))

	// A schema that has the columns is recorded, not migrated again.
	if _, err := pool.Exec(ctx, `DELETE FROM schema_migrations WHERE version = `+registrationClosureVersion); err != nil {
		t.Fatal(err)
	}
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if !recorded(t, pool, registrationClosureVersion) {
		t.Fatal("not recorded by its fingerprint")
	}
}
