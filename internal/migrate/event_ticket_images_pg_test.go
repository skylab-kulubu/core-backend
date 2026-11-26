package migrate_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
)

const eventTicketImagesVersion = "20261127190000"

// An Event's ticket image: an uploaded image's link writes its Media
// attachment (role event_ticket_image, which takes what a cover takes and
// no legacy Media), changing it moves the attachment and removing the row
// removes it; the check refuses a region out of the image. The down drops
// the table and the role and keeps every other role; Apply brings them back.
func TestEventTicketImagesMigration(t *testing.T) {
	pool := postgresPool(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	var cover, gallery, legacy, mapFits bool
	if err := pool.QueryRow(ctx, `SELECT
		media_purpose_fits_role('core', 'event_ticket_image', 'event_cover'),
		media_purpose_fits_role('core', 'event_ticket_image', 'event_gallery'),
		media_purpose_fits_role('core', 'event_ticket_image', 'legacy'),
		media_purpose_fits_role('core', 'event_map', 'event_map')`).Scan(&cover, &gallery, &legacy, &mapFits); err != nil {
		t.Fatal(err)
	}
	if !cover || !gallery || legacy || !mapFits {
		t.Fatalf("role fits: cover %v, gallery %v, legacy %v; the map's role kept %v", cover, gallery, legacy, mapFits)
	}
	eventID := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO events (id, name, location, owner_team) VALUES ($1, 'Gecekodu', 'YTÜ', 'WEBLAB')`, eventID); err != nil {
		t.Fatal(err)
	}
	first := posterMedia(t, pool, "event_cover", "image/png")
	second := posterMedia(t, pool, "event_gallery", "image/jpeg")
	attached := func(id uuid.UUID) int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM media_attachments
			WHERE media_id = $1 AND owner_service = 'core' AND owner_type = 'event' AND owner_id = $2 AND role = 'event_ticket_image'`,
			id, eventID.String()).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if _, err := pool.Exec(ctx, `INSERT INTO event_ticket_images (event_id, source, media_id, crop_x, crop_y, crop_w, crop_h)
		VALUES ($1, 'upload', $2, 0, 0.4, 1, 0.13)`, eventID, first); err != nil {
		t.Fatal(err)
	}
	if attached(first) != 1 {
		t.Fatal("the image has no attachment")
	}
	if _, err := pool.Exec(ctx, `UPDATE event_ticket_images SET media_id = $2 WHERE event_id = $1`, eventID, second); err != nil {
		t.Fatal(err)
	}
	if attached(first) != 0 || attached(second) != 1 {
		t.Fatalf("moved: first %d, second %d", attached(first), attached(second))
	}
	for _, refused := range []string{`crop_x = 0.5, crop_w = 0.6`, `crop_h = 0`, `source = 'poster'`, `source = 'cover'`} {
		if _, err := pool.Exec(ctx, `UPDATE event_ticket_images SET `+refused+` WHERE event_id = $1`, eventID); err == nil {
			t.Errorf("%s was stored", refused)
		}
	}
	if _, err := pool.Exec(ctx, `DELETE FROM event_ticket_images WHERE event_id = $1`, eventID); err != nil {
		t.Fatal(err)
	}
	if attached(second) != 0 {
		t.Fatal("the removed choice kept its attachment")
	}
	if _, err := pool.Exec(ctx, `INSERT INTO event_ticket_images (event_id, source, cover_media_id, crop_x, crop_y, crop_w, crop_h)
		VALUES ($1, 'cover', $2, 0, 0, 1, 0.2)`, eventID, uuid.New()); err != nil {
		t.Fatalf("a cover region: %v", err)
	}

	undoEventTicketImages(t, pool)
	if err := pool.QueryRow(ctx, `SELECT
		media_purpose_fits_role('core', 'event_ticket_image', 'event_cover'),
		media_purpose_fits_role('core', 'event_map', 'event_map')`).Scan(&cover, &mapFits); err != nil {
		t.Fatal(err)
	}
	if cover || !mapFits {
		t.Fatalf("after the down: the role fits %v, the map's role %v", cover, mapFits)
	}
	var table bool
	if err := pool.QueryRow(ctx, `SELECT to_regclass('public.event_ticket_images') IS NOT NULL`).Scan(&table); err != nil || table {
		t.Fatalf("table after the down: %v %v", table, err)
	}
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT media_purpose_fits_role('core', 'event_ticket_image', 'event_gallery')`).Scan(&gallery); err != nil || !gallery {
		t.Fatalf("after Apply: %v %v", gallery, err)
	}
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
}

// undoEventTicketImages rolls back the ticket image migration, whose
// triggers use the Media attachments' function, so that an older down
// migration can run; applying again brings it back.
func undoEventTicketImages(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), readDown(t, eventTicketImagesVersion+"_event_ticket_images.down.sql")); err != nil {
		t.Fatalf("event ticket images down: %v", err)
	}
	if _, err := pool.Exec(context.Background(), `DELETE FROM schema_migrations WHERE version = `+eventTicketImagesVersion); err != nil {
		t.Fatal(err)
	}
}
