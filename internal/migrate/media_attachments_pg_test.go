package migrate_test

import (
	"context"
	"io/fs"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/db"
	"github.com/skylab-kulubu/core-backend/internal/media"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

const mediaAttachmentsVersion = "20260926120000"

// TestMediaAttachmentsAttachMediaLinkedBeforeThem runs the Media attachment
// migration over Media stored before it: whatever core already links becomes
// attached with its Media attachment, the rest stays pending with no expiry.
func TestMediaAttachmentsAttachMediaLinkedBeforeThem(t *testing.T) {
	pool := postgresPool(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	down, err := fs.ReadFile(db.DownSQL, "migrations/"+mediaAttachmentsVersion+"_media_attachments.down.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, string(down)); err != nil {
		t.Fatalf("down: %v", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM schema_migrations WHERE version = `+mediaAttachmentsVersion); err != nil {
		t.Fatal(err)
	}

	person := uuid.New()
	if _, _, err := user.NewService(user.NewPostgresStore(pool)).Ensure(ctx, person, user.Profile{
		Email: "before@example.com", FirstName: "Ada", LastName: "Before", Username: "before",
	}); err != nil {
		t.Fatal(err)
	}
	stored := func(name string) uuid.UUID {
		t.Helper()
		id := uuid.New()
		if _, err := pool.Exec(ctx, `INSERT INTO media (id, file_name, file_type, file_url, file_size, uploaded_by, kind)
			VALUES ($1, $2, 'image/png', $3, 3, $4, 'IMAGE')`, id, name, "images/"+id.String(), person); err != nil {
			t.Fatal(err)
		}
		return id
	}
	cover, gallery, profile, draft, published, archivedCover, orphan, archivedOrphan :=
		stored("cover"), stored("gallery"), stored("profile"), stored("draft"), stored("published"),
		stored("archived-cover"), stored("orphan"), stored("archived-orphan")
	eventID, oldEventID := uuid.New(), uuid.New()
	templateID := uuid.New()
	for _, statement := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO events (id, name, location, owner_team, cover_image_id) VALUES ($1, 'Hack', 'YTÜ', 'WEBLAB', $2)`, []any{eventID, cover}},
		{`INSERT INTO event_images (event_id, media_id) VALUES ($1, $2)`, []any{eventID, gallery}},
		{`UPDATE users SET profile_picture_id = $2, profile_picture_url = $3 WHERE id = $1`, []any{person, profile, "images/" + profile.String()}},
		{`INSERT INTO certificate_templates (id, name, owner_team, source_kind, draft_layout)
			VALUES ($1, 'Before', 'WEBLAB', 'upload', jsonb_build_object('backgroundMediaId', $2::text, 'elements', '[]'::jsonb))`, []any{templateID, draft}},
		{`INSERT INTO certificate_template_versions (id, template_id, version, layout, asset_manifest, checksum)
			VALUES ($1, $2, 1, '{}'::jsonb, jsonb_build_object($3::text, jsonb_build_object('key', 'k', 'contentType', 'image/png')), 'v1')`, []any{uuid.New(), templateID, published}},
		// An old Event keeps a cover that was archived after it was linked.
		{`INSERT INTO events (id, name, location, owner_team, cover_image_id) VALUES ($1, 'Old', 'YTÜ', 'WEBLAB', $2)`, []any{oldEventID, archivedCover}},
		{`UPDATE media SET deleted_at = now() WHERE id = ANY ($1)`, []any{[]uuid.UUID{archivedCover, archivedOrphan}}},
	} {
		if _, err := pool.Exec(ctx, statement.sql, statement.args...); err != nil {
			t.Fatalf("%s: %v", statement.sql, err)
		}
	}

	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	up, err := fs.ReadFile(db.UpSQL, "migrations/"+mediaAttachmentsVersion+"_media_attachments.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, string(up)); err != nil {
		t.Fatalf("repeated up: %v", err)
	}

	store := media.NewPostgresStore(pool)
	status := func(id uuid.UUID) media.Media {
		t.Helper()
		got, err := store.GetIncludingDeleted(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	for name, id := range map[string]uuid.UUID{
		"cover": cover, "gallery": gallery, "profile": profile, "draft": draft, "published": published, "archived cover": archivedCover,
	} {
		if got := status(id); got.Status != media.StatusAttached || got.ExpiresAt != nil {
			t.Errorf("%s: status %q expires %v, want attached", name, got.Status, got.ExpiresAt)
		}
	}
	// Nothing core can see uses these: they are kept, and ticket 08 decides.
	for name, id := range map[string]uuid.UUID{"orphan": orphan, "archived orphan": archivedOrphan} {
		if got := status(id); got.Status != media.StatusPending || got.ExpiresAt != nil {
			t.Errorf("%s: status %q expires %v, want pending with no expiry", name, got.Status, got.ExpiresAt)
		}
	}

	// Each link got its Media attachment: removing the link detaches the Media.
	if _, err := pool.Exec(ctx, `UPDATE events SET cover_image_id = NULL WHERE id = $1`, eventID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM event_images WHERE event_id = $1`, eventID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE users SET profile_picture_id = NULL WHERE id = $1`, person); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE certificate_templates SET draft_layout = '{}'::jsonb WHERE id = $1`, templateID); err != nil {
		t.Fatal(err)
	}
	// They are legacy: still in use outside core, perhaps, so no expiry.
	for name, id := range map[string]uuid.UUID{"cover": cover, "gallery": gallery, "profile": profile, "draft": draft} {
		if got := status(id); got.Status != media.StatusDetached || got.ExpiresAt != nil {
			t.Errorf("%s after unlinking: status %q expires %v, want detached with no expiry", name, got.Status, got.ExpiresAt)
		}
	}

	var unusedColumn bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM information_schema.columns
		WHERE table_schema = 'public' AND table_name = 'media' AND column_name = 'attached')`).Scan(&unusedColumn); err != nil {
		t.Fatal(err)
	}
	if unusedColumn {
		t.Error("media.attached, unused since the first migration, is still there beside status")
	}
}

// A database that lost one attachment trigger is not taken for migrated: the
// migration runs again and puts it back.
func TestApplyRepairsAMissingMediaAttachmentTrigger(t *testing.T) {
	pool := postgresPool(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `DROP TRIGGER users_media_attachments_update ON users`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `DROP TABLE schema_migrations`); err != nil {
		t.Fatal(err)
	}
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	var restored bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_trigger
		WHERE tgrelid = 'users'::regclass AND tgname = 'users_media_attachments_update')`).Scan(&restored); err != nil {
		t.Fatal(err)
	}
	if !restored {
		t.Fatal("the profile picture attachment trigger was not restored")
	}
}
