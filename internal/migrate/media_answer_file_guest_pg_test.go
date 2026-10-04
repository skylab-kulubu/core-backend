package migrate_test

import (
	"context"
	"io/fs"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/skylab-kulubu/core-backend/db"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
)

const mediaAnswerFileGuestVersion = "20261004120000"

// guestAnswerFileHolds reports whether the database takes a staged upload of
// no one and lets Skyforms' answer role take a guest Answer file.
func guestAnswerFileHolds(t *testing.T, pool *pgxpool.Pool) bool {
	t.Helper()
	var nullable string
	var fits bool
	if err := pool.QueryRow(context.Background(), `SELECT
		(SELECT is_nullable FROM information_schema.columns
			WHERE table_schema = 'public' AND table_name = 'media_upload_staging' AND column_name = 'subject_id'),
		media_purpose_fits_role('forms', 'answer', 'answer_file_guest')`, pgx.QueryExecModeSimpleProtocol).
		Scan(&nullable, &fits); err != nil {
		t.Fatal(err)
	}
	return nullable == "YES" && fits
}

// A staged upload of no one is stored with a NULL subject; the account
// reference guard passes it, and the guard still refuses a subject with no
// active account.
func TestGuestAnswerFileStagesAnUploadOfNoOne(t *testing.T) {
	pool := postgresPool(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if !guestAnswerFileHolds(t, pool) {
		t.Fatal("the guest Answer file migration did not hold after Apply")
	}
	if _, err := pool.Exec(ctx, `INSERT INTO media_upload_staging (object_key, subject_id, cleanup_after) VALUES ($1, NULL, $2)`,
		"private/files/"+uuid.NewString(), time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("staged upload of no one: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO media_upload_staging (object_key, subject_id, cleanup_after) VALUES ($1, $2, $3)`,
		"private/files/"+uuid.NewString(), uuid.New(), time.Now().Add(time.Hour)); err == nil {
		t.Fatal("a staged upload of a subject with no account was stored")
	}
}

// The down migration refuses while a staged upload of no one is left. Once
// none is, it puts back the NOT NULL subject and the role table without the
// guest Answer file; applying again brings both back.
func TestGuestAnswerFileDownRefusesWhileAnUploadOfNoOneIsLeft(t *testing.T) {
	pool := postgresPool(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	down, err := fs.ReadFile(db.DownSQL, "migrations/"+mediaAnswerFileGuestVersion+"_media_answer_file_guest.down.sql")
	if err != nil {
		t.Fatal(err)
	}
	key := "private/files/" + uuid.NewString()
	if _, err := pool.Exec(ctx, `INSERT INTO media_upload_staging (object_key, subject_id, cleanup_after) VALUES ($1, NULL, $2)`,
		key, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, string(down)); err == nil || !strings.Contains(err.Error(), "media answer_file_guest down") {
		t.Fatalf("down while an upload of no one is staged: %v", err)
	}
	if !guestAnswerFileHolds(t, pool) {
		t.Fatal("a refused down changed the schema")
	}

	if _, err := pool.Exec(ctx, `DELETE FROM media_upload_staging WHERE object_key = $1`, key); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, string(down)); err != nil {
		t.Fatalf("down once nothing is left: %v", err)
	}
	if guestAnswerFileHolds(t, pool) {
		t.Fatal("after down the guest Answer file still holds")
	}
	if !postersHold(t, pool) || !framesHold(t, pool) {
		t.Fatal("the guest Answer file's down took the posters' or frames' roles with it")
	}
	if _, err := pool.Exec(ctx, `DELETE FROM schema_migrations WHERE version = `+mediaAnswerFileGuestVersion); err != nil {
		t.Fatal(err)
	}
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if !guestAnswerFileHolds(t, pool) {
		t.Fatal("applying again after down did not bring the guest Answer file back")
	}
}
