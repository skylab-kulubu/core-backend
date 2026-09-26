package migrate_test

import (
	"context"
	"io/fs"
	"testing"
	"time"

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

// TestMediaAttachmentOwnerIDsAreTheProductsOwn: after the owner id became
// text, core's own links still write and remove their Media attachments with
// their records' UUIDs, and another product's record id that is no UUID (a
// CMS page as clientId:slug) is one link per Media, owner and role.
func TestMediaAttachmentOwnerIDsAreTheProductsOwn(t *testing.T) {
	pool := postgresPool(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	person := uuid.New()
	if _, _, err := user.NewService(user.NewPostgresStore(pool)).Ensure(ctx, person, user.Profile{
		Email: "owner@example.com", FirstName: "Ada", LastName: "Owner", Username: "owner",
	}); err != nil {
		t.Fatal(err)
	}
	stored := func() uuid.UUID {
		t.Helper()
		id := uuid.New()
		if _, err := pool.Exec(ctx, `INSERT INTO media (id, file_name, file_type, file_url, file_size, uploaded_by, kind)
			VALUES ($1, 'x.png', 'image/png', $2, 3, $3, 'IMAGE')`, id, "images/"+id.String(), person); err != nil {
			t.Fatal(err)
		}
		return id
	}
	cover, logo := stored(), stored()
	eventID := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO events (id, name, location, owner_team, cover_image_id) VALUES ($1, 'Hack', 'YTÜ', 'WEBLAB', $2)`, eventID, cover); err != nil {
		t.Fatal(err)
	}
	var ownerID string
	if err := pool.QueryRow(ctx, `SELECT owner_id FROM media_attachments WHERE media_id = $1`, cover).Scan(&ownerID); err != nil {
		t.Fatal(err)
	}
	if ownerID != eventID.String() {
		t.Fatalf("core's owner id %q, want %s", ownerID, eventID)
	}
	if _, err := pool.Exec(ctx, `UPDATE events SET cover_image_id = NULL WHERE id = $1`, eventID); err != nil {
		t.Fatal(err)
	}
	var left int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM media_attachments WHERE media_id = $1`, cover).Scan(&left); err != nil || left != 0 {
		t.Fatalf("core link removed, %d Media attachments left (err %v)", left, err)
	}

	insert := `INSERT INTO media_attachments (media_id, owner_service, owner_type, owner_id, role)
		VALUES ($1, 'cms', 'page', 'skylab-site:hakkimizda', 'image')`
	if _, err := pool.Exec(ctx, insert, logo); err != nil {
		t.Fatalf("CMS page owner id: %v", err)
	}
	if _, err := pool.Exec(ctx, insert, logo); err == nil {
		t.Fatal("the same link was written twice")
	}
	if _, err := pool.Exec(ctx, `INSERT INTO media_attachments (media_id, owner_service, owner_type, owner_id, role)
		VALUES ($1, 'cms', 'page', '', 'image')`, logo); err == nil {
		t.Fatal("an empty owner id was accepted")
	}
}

// A rerun of the Media attachment migration (a lost trigger) puts back its
// UUID comparison; the owner id migration's fingerprint notices and runs it
// again, so core's links keep removing their Media attachments.
func TestApplyRepairsTheTextOwnerComparisonAfterARerun(t *testing.T) {
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

	person := uuid.New()
	if _, _, err := user.NewService(user.NewPostgresStore(pool)).Ensure(ctx, person, user.Profile{
		Email: "rerun@example.com", FirstName: "Ada", LastName: "Rerun", Username: "rerun",
	}); err != nil {
		t.Fatal(err)
	}
	picture := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO media (id, file_name, file_type, file_url, file_size, uploaded_by, kind)
		VALUES ($1, 'me.png', 'image/png', $2, 3, $3, 'IMAGE')`, picture, "images/"+picture.String(), person); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`UPDATE users SET profile_picture_id = $2 WHERE id = $1`,
		`UPDATE users SET profile_picture_id = NULL WHERE id = $1 AND $2::uuid IS NOT NULL`,
	} {
		if _, err := pool.Exec(ctx, statement, person, picture); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM media WHERE id = $1`, picture).Scan(&status); err != nil || status != "detached" {
		t.Fatalf("profile picture after unlinking: status %q (err %v), want detached", status, err)
	}
}

// A rerun of the Media attachment migration puts back its status and
// current-media functions, without the legacy backfill's hold and purpose
// check; the backfill migration's fingerprint notices and runs it again.
func TestApplyRepairsTheDetachExpiryHoldAfterARerun(t *testing.T) {
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

	person := uuid.New()
	if _, _, err := user.NewService(user.NewPostgresStore(pool)).Ensure(ctx, person, user.Profile{
		Email: "held@example.com", FirstName: "Ada", LastName: "Held", Username: "held",
	}); err != nil {
		t.Fatal(err)
	}
	picture := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO media (id, file_name, file_type, file_url, file_size, uploaded_by, kind, purpose, detach_expiry_held)
		VALUES ($1, 'me.png', 'image/png', $2, 3, $3, 'IMAGE', 'profile_picture', true)`, picture, "images/"+picture.String(), person); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`UPDATE users SET profile_picture_id = $2 WHERE id = $1`,
		`UPDATE users SET profile_picture_id = NULL WHERE id = $1 AND $2::uuid IS NOT NULL`,
	} {
		if _, err := pool.Exec(ctx, statement, person, picture); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	var status string
	var expires *time.Time
	if err := pool.QueryRow(ctx, `SELECT status, expires_at FROM media WHERE id = $1`, picture).Scan(&status, &expires); err != nil || status != "detached" || expires != nil {
		t.Fatalf("held picture after unlinking: status %q expires %v (err %v), want detached with no expiry", status, expires, err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO media_attachments (media_id, owner_service, owner_type, owner_id, role)
		VALUES ($1, 'cms', 'page', 'skylab-site:hakkimizda', 'image')`, picture); err == nil {
		t.Fatal("a profile picture was attached as a CMS image")
	}
}
