package migrate_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
)

const eventLocationPointVersion = "20261122150000"

func eventsLocationColumns(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `
		SELECT count(*) FROM information_schema.columns
		WHERE table_schema = 'public' AND table_name = 'events'
		  AND column_name IN ('location_lat', 'location_lng', 'location_address')`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// An Event's point and address come as two nullable columns and a text
// with an empty default, under one check. An Event already there gets no
// point; one written by the release before (which names none of the
// columns) gets none either. The down drops them and Apply brings them
// back.
func TestEventLocationPointMigration(t *testing.T) {
	pool := postgresPool(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if got := eventsLocationColumns(t, pool); got != 3 {
		t.Fatalf("columns after Apply: %d", got)
	}
	down := readDown(t, eventLocationPointVersion+"_event_location_point.down.sql")
	if _, err := pool.Exec(ctx, down); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM schema_migrations WHERE version = `+eventLocationPointVersion); err != nil {
		t.Fatal(err)
	}
	if got := eventsLocationColumns(t, pool); got != 0 {
		t.Fatalf("columns after the down: %d", got)
	}
	before := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO events (id, name, location, owner_team) VALUES ($1, 'Gecekodu', 'YTÜ', 'WEBLAB')`, before); err != nil {
		t.Fatal(err)
	}
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	var lat, lng *float64
	var address string
	if err := pool.QueryRow(ctx, `SELECT location_lat, location_lng, location_address FROM events WHERE id = $1`, before).
		Scan(&lat, &lng, &address); err != nil || lat != nil || lng != nil || address != "" {
		t.Fatalf("an Event from before: %v %v %q %v", lat, lng, address, err)
	}
	// The release before writes an Event naming none of the columns.
	old := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO events (id, name, location, owner_team) VALUES ($1, 'Söyleşi', 'Online', 'WEBLAB')`, old); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE events SET location_lat = 41.025376, location_lng = 28.889648, location_address = 'Esenler' WHERE id = $1`, before); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE events SET location_lng = NULL WHERE id = $1`, before); err == nil {
		t.Fatal("half a point was stored")
	}
	// Recorded already: Apply does not run it again, and keeps the point.
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT location_lat, location_lng, location_address FROM events WHERE id = $1`, before).
		Scan(&lat, &lng, &address); err != nil || lat == nil || *lat != 41.025376 || lng == nil || *lng != 28.889648 || address != "Esenler" {
		t.Fatalf("after a second Apply: %v %v %q %v", lat, lng, address, err)
	}
}
