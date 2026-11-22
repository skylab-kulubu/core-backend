package migrate_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
)

const mapRegionsVersion = "20261122170000"

// The regions' table refuses a box that is not one, a status it does not
// know, a ready region without its object, and a second live region of the
// same box (a failed or retired one does not count); the down drops it and
// Apply brings it back.
func TestMapRegionsMigration(t *testing.T) {
	pool := postgresPool(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	insert := func(box [4]float64, status string) error {
		_, err := pool.Exec(ctx, `INSERT INTO map_regions (id, name, min_lon, min_lat, max_lon, max_lat, status) VALUES ($1, 'İstanbul', $2, $3, $4, $5, $6)`,
			uuid.New(), box[0], box[1], box[2], box[3], status)
		return err
	}
	istanbul := [4]float64{27.95, 40.80, 29.95, 41.60}
	if err := insert(istanbul, "requested"); err != nil {
		t.Fatal(err)
	}
	for name, err := range map[string]error{
		"the same live box":      insert(istanbul, "requested"),
		"upside down":            insert([4]float64{29, 41, 28, 42}, "requested"),
		"off the map":            insert([4]float64{27, 86, 28, 87}, "requested"),
		"an unknown status":      insert([4]float64{27, 40, 28, 41}, "deleted"),
		"ready without a object": insert([4]float64{27, 40, 28, 41}, "ready"),
	} {
		if err == nil {
			t.Errorf("%s: stored", name)
		}
	}
	if err := insert(istanbul, "failed"); err != nil {
		t.Fatalf("a failed region of a live box: %v", err)
	}
	if _, err := pool.Exec(ctx, readDown(t, mapRegionsVersion+"_map_regions.down.sql")); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM schema_migrations WHERE version = `+mapRegionsVersion); err != nil {
		t.Fatal(err)
	}
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if err := insert(istanbul, "requested"); err != nil {
		t.Fatalf("after Apply again: %v", err)
	}
}
