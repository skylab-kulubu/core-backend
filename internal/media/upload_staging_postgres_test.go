package media_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/media"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
	"github.com/skylab-kulubu/core-backend/internal/testpostgres"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

func TestPostgresUploadRejectsInactiveSubjectBeforeR2Put(t *testing.T) {
	pool := testpostgres.Start(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	users := user.NewPostgresStore(pool)
	uploaderID := uuid.New()
	if _, _, err := user.NewService(users).Ensure(ctx, uploaderID, user.Profile{Email: "rejected-upload@example.test"}); err != nil {
		t.Fatal(err)
	}
	if _, err := users.RequestDeletion(ctx, uploaderID, nil); err != nil {
		t.Fatal(err)
	}

	blobs := &recordingBlobStore{}
	store := media.NewPostgresStore(pool)
	svc := media.NewServiceWithOptions(store, blobs, authz.NewAuthorizer(authz.DefaultPolicy()), "", media.ServiceOptions{
		UploadStagingGrace: 2 * time.Minute,
	})
	_, err := svc.Upload(ctx, authz.Principal{ID: uploaderID.String()}, "rejected.png", "image/png", pngDot())
	if !errors.Is(err, media.ErrForbidden) {
		t.Fatalf("upload error = %v", err)
	}
	if len(blobs.objects) != 0 || len(blobs.deleted) != 0 {
		t.Fatalf("inactive upload touched R2: objects=%d deleted=%v", len(blobs.objects), blobs.deleted)
	}
	var staged int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM media_upload_staging`).Scan(&staged); err != nil || staged != 0 {
		t.Fatalf("staged intents=%d err=%v", staged, err)
	}
}

func TestStagingSweeperNeverDeletesPublishedMedia(t *testing.T) {
	pool := testpostgres.Start(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	users := user.NewPostgresStore(pool)
	uploaderID := uuid.New()
	if _, _, err := user.NewService(users).Ensure(ctx, uploaderID, user.Profile{Email: "published-upload@example.test"}); err != nil {
		t.Fatal(err)
	}
	store := media.NewPostgresStore(pool)
	key := "images/published-staging-check"
	blobs := &recordingBlobStore{}
	if err := blobs.Put(ctx, key, pngDot(), media.BlobMetadata{ContentType: "image/png"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(ctx, media.Media{
		Name: "published.png", Type: "image/png", Key: key, Kind: media.KindImage, UploadedBy: uploaderID,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.StageUpload(ctx, key, uploaderID, time.Now().UTC().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}

	report, err := media.PurgeStagedUploads(ctx, store, blobs, time.Now().UTC(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if report.Scanned != 1 || report.Resolved != 1 {
		t.Fatalf("report=%+v", report)
	}
	if _, ok := blobs.objects[key]; !ok {
		t.Fatal("sweeper deleted an object referenced by published media")
	}
	var staged int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM media_upload_staging WHERE object_key=$1`, key).Scan(&staged); err != nil || staged != 0 {
		t.Fatalf("stale staging intent count=%d err=%v", staged, err)
	}
}

func TestStagingSweeperPersistsBoundedRetryAfterR2Failure(t *testing.T) {
	pool := testpostgres.Start(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	store := media.NewPostgresStore(pool)
	users := user.NewPostgresStore(pool)
	uploaderID := uuid.New()
	if _, _, err := user.NewService(users).Ensure(ctx, uploaderID, user.Profile{Email: "abandoned-upload@example.test"}); err != nil {
		t.Fatal(err)
	}
	key := "files/abandoned-upload"
	now := time.Now().UTC()
	if err := store.StageUpload(ctx, key, uploaderID, now.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	deleteErr := errors.New("temporary r2 failure")
	blobs := &recordingBlobStore{deleteErr: deleteErr}
	if err := blobs.Put(ctx, key, []byte("orphan"), media.BlobMetadata{ContentType: "application/octet-stream"}); err != nil {
		t.Fatal(err)
	}

	report, err := media.PurgeStagedUploads(ctx, store, blobs, now, 25)
	if !errors.Is(err, deleteErr) || report.Scanned != 1 || report.Resolved != 0 {
		t.Fatalf("first report=%+v err=%v", report, err)
	}
	var attempts int
	var retryAt time.Time
	var errorCode string
	if err := pool.QueryRow(ctx, `
		SELECT attempt_count, cleanup_after, last_error_code
		FROM media_upload_staging WHERE object_key=$1
	`, key).Scan(&attempts, &retryAt, &errorCode); err != nil {
		t.Fatal(err)
	}
	if attempts != 1 || !retryAt.After(now) || errorCode != "blob_delete_failed" {
		t.Fatalf("attempts=%d retryAt=%s code=%q", attempts, retryAt, errorCode)
	}

	blobs.deleteErr = nil
	report, err = media.PurgeStagedUploads(ctx, store, blobs, retryAt.Add(time.Second), 25)
	if err != nil || report.Resolved != 1 {
		t.Fatalf("retry report=%+v err=%v", report, err)
	}
	if _, ok := blobs.objects[key]; ok {
		t.Fatal("retry left abandoned object")
	}
}
