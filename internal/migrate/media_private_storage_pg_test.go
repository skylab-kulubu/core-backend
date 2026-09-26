package migrate_test

import (
	"context"
	"io/fs"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/db"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// TestApplyRepairsAMissingMediaAccessLog: a database that lost part of the
// private Media schema is not taken as migrated; the migration runs again.
func TestApplyRepairsAMissingMediaAccessLog(t *testing.T) {
	pool := postgresPool(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `DROP TABLE media_read_link_opens; DROP TABLE schema_migrations`); err != nil {
		t.Fatal(err)
	}
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	var restored bool
	if err := pool.QueryRow(ctx, `SELECT to_regclass('public.media_read_link_opens') IS NOT NULL`).Scan(&restored); err != nil || !restored {
		t.Fatalf("access log opens restored %v, err %v", restored, err)
	}
}

// mediaVisibilityCheck is media_visibility_check as PostgreSQL prints it.
const mediaVisibilityCheck = "CHECK ((((visibility = 'public'::text) AND (encryption_algorithm IS NULL) AND (wrapped_data_key IS NULL) AND (key_version IS NULL)) OR ((visibility = 'private'::text) AND (encryption_algorithm IS NOT NULL) AND (wrapped_data_key IS NOT NULL) AND (key_version IS NOT NULL) AND (key_version >= 1))))"

// TestApplyRepairsAPermissiveMediaVisibilityCheck: a check that lets a
// private Media lose its key version (or anything else) is not taken as the
// migrated schema; the migration runs again and puts the exact one back.
func TestApplyRepairsAPermissiveMediaVisibilityCheck(t *testing.T) {
	pool := postgresPool(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		DELETE FROM schema_migrations WHERE version = 20260926170000;
		ALTER TABLE media
			DROP CONSTRAINT media_visibility_check,
			ADD CONSTRAINT media_visibility_check CHECK (visibility IN ('public', 'private') OR key_version >= 1);
	`); err != nil {
		t.Fatal(err)
	}

	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	var definition string
	if err := pool.QueryRow(ctx, `
		SELECT pg_get_constraintdef(oid) FROM pg_constraint
		WHERE conrelid = 'media'::regclass AND conname = 'media_visibility_check'`).Scan(&definition); err != nil {
		t.Fatal(err)
	}
	if definition != mediaVisibilityCheck {
		t.Fatalf("media_visibility_check is %s", definition)
	}
}

// A private Media without a key version is refused.
func TestPrivateMediaNeedsAKeyVersion(t *testing.T) {
	pool := postgresPool(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	uploader := uuid.New()
	if _, _, err := user.NewService(user.NewPostgresStore(pool)).Ensure(ctx, uploader, user.Profile{Email: "uploader@example.test"}); err != nil {
		t.Fatal(err)
	}
	_, err := pool.Exec(ctx, `
		INSERT INTO media (id, file_name, file_type, file_url, file_size, uploaded_by, kind, purpose, visibility, encryption_algorithm, wrapped_data_key)
		VALUES ($1, 'cv.pdf', 'application/pdf', 'private/files/cv', 10, $2, 'FILE', 'answer_file', 'private', 'aes-256-gcm-chunked-v1', 'vault:v1:AAAA')`,
		uuid.New(), uploader)
	if err == nil || !strings.Contains(err.Error(), "media_visibility_check") {
		t.Fatalf("err = %v", err)
	}
}

// TestApplyRepairsAMissingReadLinkSubjectGuard: the guard on the person a
// read link names comes back if it goes missing.
func TestApplyRepairsAMissingReadLinkSubjectGuard(t *testing.T) {
	pool := postgresPool(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		DELETE FROM schema_migrations WHERE version = 20260926171000;
		DROP TRIGGER media_read_links_require_active_subject ON media_read_links;`); err != nil {
		t.Fatal(err)
	}
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	var restored bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_trigger
		WHERE tgrelid = 'media_read_links'::regclass AND tgname = 'media_read_links_require_active_subject')`).Scan(&restored); err != nil || !restored {
		t.Fatalf("guard restored %v, err %v", restored, err)
	}
}

// TestMediaPrivateStorageDownRefusesToLoseWrappedKeys: rolling the schema
// back would drop the only copy of every private Media's data key.
func TestMediaPrivateStorageDownRefusesToLoseWrappedKeys(t *testing.T) {
	pool := postgresPool(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	uploader := uuid.New()
	if _, _, err := user.NewService(user.NewPostgresStore(pool)).Ensure(ctx, uploader, user.Profile{Email: "uploader@example.test"}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO media (id, file_name, file_type, file_url, file_size, uploaded_by, kind, purpose, visibility, encryption_algorithm, wrapped_data_key, key_version)
		VALUES ($1, 'cv.pdf', 'application/pdf', 'private/files/cv', 10, $2, 'FILE', 'answer_file', 'private', 'aes-256-gcm-chunked-v1', 'vault:v1:AAAA', 1)`,
		uuid.New(), uploader); err != nil {
		t.Fatal(err)
	}
	down, err := fs.ReadFile(db.DownSQL, "migrations/20260926170000_media_private_storage.down.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, string(down)); err == nil || !strings.Contains(err.Error(), "private Media exist") {
		t.Fatalf("down migration with a private Media: err = %v", err)
	}
}
