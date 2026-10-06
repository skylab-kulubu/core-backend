package migrate_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/skylab-kulubu/core-backend/internal/media"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// TestCertificateLayoutGuardChecksOnlyTheReferencesAnEditIntroduces: a
// certificate template whose draft links a Media archived after it was
// linked can still be edited while that link stays; a reference the edit adds
// or swaps in must be a current Media, not an archived or purging one. A
// published version's layout and manifest follow the same rule.
func TestCertificateLayoutGuardChecksOnlyTheReferencesAnEditIntroduces(t *testing.T) {
	pool := postgresPool(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	owner := uuid.New()
	if _, _, err := user.NewService(user.NewPostgresStore(pool)).Ensure(ctx, owner, user.Profile{
		Email: "layout-owner@example.com", FirstName: "Layout", LastName: "Owner", Username: "layout-owner",
	}); err != nil {
		t.Fatal(err)
	}
	store := media.NewPostgresStore(pool)
	stored := func(name string) uuid.UUID {
		t.Helper()
		item, err := store.Create(ctx, media.Media{Name: name + ".png", Type: "image/png", Kind: media.KindImage, Key: "images/" + name, UploadedBy: owner})
		if err != nil {
			t.Fatal(err)
		}
		return item.ID
	}
	background, logo, current, archived, purging := stored("background"), stored("logo"), stored("current"), stored("archived"), stored("purging")

	templateID := uuid.New()
	if _, err := pool.Exec(ctx, `
		INSERT INTO certificate_templates (id, name, owner_team, source_kind, draft_layout, created_by)
		VALUES ($1, 'Template', 'WEBLAB', 'upload', jsonb_build_object(
			'backgroundMediaId', $2::text,
			'elements', jsonb_build_array(jsonb_build_object('type', 'image', 'mediaId', $3::text))), $4)`,
		templateID, background, logo, owner); err != nil {
		t.Fatal(err)
	}
	versionID := uuid.New()
	if _, err := pool.Exec(ctx, `
		INSERT INTO certificate_template_versions (id, template_id, version, layout, asset_manifest, checksum)
		VALUES ($1, $2, 1, jsonb_build_object('backgroundMediaId', $3::text),
			jsonb_build_object($3::text, jsonb_build_object('key', 'k', 'contentType', 'image/png')), 'v1')`,
		versionID, templateID, background); err != nil {
		t.Fatal(err)
	}

	// Archived after they were linked; another Media is archived, and one is
	// being purged, before anything links them.
	for _, id := range []uuid.UUID{background, logo, archived} {
		if err := store.Archive(ctx, id, &owner); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(ctx, `UPDATE media SET deleted_at = $2, blob_purge_started_at = now() WHERE id = $1`,
		purging, time.Now().UTC().Add(-31*24*time.Hour)); err != nil {
		t.Fatal(err)
	}

	setDraft := func(background, element uuid.UUID) error {
		t.Helper()
		_, err := pool.Exec(ctx, `
			UPDATE certificate_templates SET name = 'Edited', draft_layout = jsonb_build_object(
				'backgroundMediaId', $2::text,
				'elements', jsonb_build_array(
					jsonb_build_object('type', 'text', 'text', 'Edited'),
					jsonb_build_object('type', 'image', 'mediaId', $3::text)))
			WHERE id = $1`, templateID, background, element)
		return err
	}
	notCurrent := func(err error) bool {
		var pg *pgconn.PgError
		return errors.As(err, &pg) && pg.Code == "23503"
	}

	if err := setDraft(background, logo); err != nil {
		t.Fatalf("an edit that keeps the archived background and logo: %v", err)
	}
	if err := setDraft(background, archived); !notCurrent(err) {
		t.Fatalf("an edit that adds an archived Media: err = %v, want 23503", err)
	}
	if err := setDraft(background, purging); !notCurrent(err) {
		t.Fatalf("an edit that adds a purging Media: err = %v, want 23503", err)
	}
	if err := setDraft(archived, logo); !notCurrent(err) {
		t.Fatalf("an edit that replaces the background with an archived Media: err = %v, want 23503", err)
	}
	if err := setDraft(current, logo); err != nil {
		t.Fatalf("an edit that replaces the background with a current Media: %v", err)
	}
	// The archived background left the draft: linking it again is a new
	// reference.
	if err := setDraft(background, logo); !notCurrent(err) {
		t.Fatalf("an edit that links the archived background again: err = %v, want 23503", err)
	}

	if _, err := pool.Exec(ctx, `UPDATE certificate_template_versions
		SET layout = layout || '{"elements": []}'::jsonb WHERE id = $1`, versionID); err != nil {
		t.Fatalf("a version edit that keeps its archived asset: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE certificate_template_versions
		SET asset_manifest = asset_manifest || jsonb_build_object($2::text, jsonb_build_object('key', 'k2', 'contentType', 'image/png'))
		WHERE id = $1`, versionID, archived); !notCurrent(err) {
		t.Fatalf("a version edit that adds an archived asset: err = %v, want 23503", err)
	}
}
