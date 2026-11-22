package migrate_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
)

const eventMapImageVersion = "20261122160000"

// An Event's map: its column links an event_map Media, whose Media
// attachment the statement writes (role event_map, which takes no other
// purpose and no legacy Media); clearing it removes the attachment. The
// down drops the columns and the attachments, and Apply brings them back.
func TestEventMapImageMigration(t *testing.T) {
	pool := postgresPool(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	var fits, legacy, cover bool
	if err := pool.QueryRow(ctx, `SELECT
		media_purpose_fits_role('core', 'event_map', 'event_map'),
		media_purpose_fits_role('core', 'event_map', 'legacy'),
		media_purpose_fits_role('core', 'event_map', 'event_cover')`).Scan(&fits, &legacy, &cover); err != nil {
		t.Fatal(err)
	}
	if !fits || legacy || cover {
		t.Fatalf("role fits: event_map %v, legacy %v, event_cover %v", fits, legacy, cover)
	}
	eventID := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO events (id, name, location, owner_team, location_lat, location_lng) VALUES ($1, 'Gecekodu', 'YTÜ', 'WEBLAB', 41.025376, 28.889648)`, eventID); err != nil {
		t.Fatal(err)
	}
	mapID := posterMedia(t, pool, "event_map", "image/jpeg")
	attached := func() (n int, status string) {
		t.Helper()
		if err := pool.QueryRow(ctx, `SELECT
			(SELECT count(*) FROM media_attachments WHERE media_id = $1 AND owner_service = 'core' AND owner_type = 'event' AND owner_id = $2 AND role = 'event_map'),
			(SELECT status FROM media WHERE id = $1)`, mapID, eventID.String()).Scan(&n, &status); err != nil {
			t.Fatal(err)
		}
		return n, status
	}
	if _, err := pool.Exec(ctx, `UPDATE events SET map_media_id = $2 WHERE id = $1`, eventID, mapID); err != nil {
		t.Fatal(err)
	}
	if n, status := attached(); n != 1 || status != "attached" {
		t.Fatalf("linked: %d attachments, %s", n, status)
	}
	if _, err := pool.Exec(ctx, `UPDATE events SET map_claim_id = gen_random_uuid() WHERE id = $1`, eventID); err == nil {
		t.Fatal("a claim without its lease was stored")
	}
	if _, err := pool.Exec(ctx, `UPDATE events SET map_media_id = NULL WHERE id = $1`, eventID); err != nil {
		t.Fatal(err)
	}
	if n, status := attached(); n != 0 || status != "detached" {
		t.Fatalf("cleared: %d attachments, %s", n, status)
	}
	if _, err := pool.Exec(ctx, `UPDATE events SET map_media_id = $2 WHERE id = $1`, eventID, mapID); err != nil {
		t.Fatal(err)
	}

	down := readDown(t, eventMapImageVersion+"_event_map_image.down.sql")
	if _, err := pool.Exec(ctx, down); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM schema_migrations WHERE version = `+eventMapImageVersion); err != nil {
		t.Fatal(err)
	}
	if n, _ := attached(); n != 0 {
		t.Fatalf("the down left %d attachments", n)
	}
	var columns int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns WHERE table_schema = 'public' AND table_name = 'events' AND column_name LIKE 'map\_%'`).Scan(&columns); err != nil || columns != 0 {
		t.Fatalf("columns after the down: %d %v", columns, err)
	}
	if err := pool.QueryRow(ctx, `SELECT media_purpose_fits_role('core', 'event_map', 'event_map')`).Scan(&fits); err != nil || fits {
		t.Fatalf("the down kept the role: %v %v", fits, err)
	}
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns WHERE table_schema = 'public' AND table_name = 'events' AND column_name LIKE 'map\_%'`).Scan(&columns); err != nil || columns != 6 {
		t.Fatalf("columns after Apply: %d %v", columns, err)
	}
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
}

// undoEventMapImage rolls back the map migration, whose triggers use the
// Media attachments' function and whose role tables an older down
// replaces, so that an older down migration can run; applying again brings
// it back.
func undoEventMapImage(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), readDown(t, eventMapImageVersion+"_event_map_image.down.sql")); err != nil {
		t.Fatalf("event map image down: %v", err)
	}
	if _, err := pool.Exec(context.Background(), `DELETE FROM schema_migrations WHERE version = `+eventMapImageVersion); err != nil {
		t.Fatal(err)
	}
}
