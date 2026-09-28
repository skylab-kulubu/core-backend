package media_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/media"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
	"github.com/skylab-kulubu/core-backend/internal/testpostgres"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

func directUploadRecord(uploader uuid.UUID, expires time.Time) media.DirectUploadRecord {
	id := uuid.New()
	return media.DirectUploadRecord{
		ID: id, UploaderID: uploader, Purpose: "club_file", Name: "kişisel notlar.pdf", Size: 100, MaxBytes: 1 << 30,
		Types: []string{"application/pdf"}, PartSize: 16 << 20, Key: "pending/" + id.String(), ExpiresAt: expires,
	}
}

// A person being erased cannot start a Direct upload, and one they started
// before goes with their staged uploads (erase_staged_uploads): once it
// expires, its multipart upload is aborted and its record, file name
// included, is deleted.
func TestDirectUploadsFollowTheirUploadersAccount(t *testing.T) {
	pool := testpostgres.Start(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	users := user.NewPostgresStore(pool)
	uploader := uuid.New()
	if _, _, err := user.NewService(users).Ensure(ctx, uploader, user.Profile{Email: "direct-upload@example.test"}); err != nil {
		t.Fatal(err)
	}
	store := media.NewPostgresStore(pool)
	r2, fake := multipartR2(t)
	now := time.Now().UTC()
	rec := directUploadRecord(uploader, now.Add(media.DirectUploadTTL))
	if err := store.StageDirectUpload(ctx, rec); err != nil {
		t.Fatal(err)
	}
	multipartID, err := r2.CreateMultipart(ctx, rec.Key)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetDirectUploadMultipart(ctx, rec.ID, multipartID); err != nil {
		t.Fatal(err)
	}
	got, err := store.GetDirectUpload(ctx, rec.ID)
	if err != nil || got.UploaderID != uploader || got.MultipartID != multipartID || got.Name != rec.Name ||
		!slices.Equal(got.Types, rec.Types) || !got.ExpiresAt.Equal(rec.ExpiresAt.Truncate(time.Microsecond)) {
		t.Fatalf("record %+v err %v", got, err)
	}

	if _, err := users.RequestDeletion(ctx, uploader, nil); err != nil {
		t.Fatal(err)
	}
	if err := store.StageDirectUpload(ctx, directUploadRecord(uploader, now.Add(time.Hour))); !errors.Is(err, media.ErrForbidden) {
		t.Fatalf("an upload started while the account is erased: %v", err)
	}

	eraser := media.NewImmediateBlobEraser(store, media.Buckets{Public: r2})
	// Before its expiry the upload is a live lease: the erasure step waits.
	if err := eraser.EnsureSubjectUploadsErased(ctx, uploader, now); err == nil {
		t.Fatal("the erasure did not wait for an open Direct upload")
	}
	if _, err := store.GetDirectUpload(ctx, rec.ID); err != nil {
		t.Fatalf("the waiting erasure removed the upload: %v", err)
	}
	if err := eraser.EnsureSubjectUploadsErased(ctx, uploader, rec.ExpiresAt); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetDirectUpload(ctx, rec.ID); !errors.Is(err, media.ErrNotFound) {
		t.Fatalf("record after the erasure: %v", err)
	}
	if aborted := fake.Aborted(); !slices.Equal(aborted, []string{rec.Key}) || len(fake.OpenUploads("media")) != 0 {
		t.Fatalf("aborted %v, open %v", aborted, fake.OpenUploads("media"))
	}
	var names int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM media_direct_uploads WHERE file_name = $1`, rec.Name).Scan(&names); err != nil || names != 0 {
		t.Fatalf("file names left %d err %v", names, err)
	}
}

// An upload ended (published or dropped) is gone for every later end.
func TestDirectUploadEndsOnce(t *testing.T) {
	pool := testpostgres.Start(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	uploader := uuid.New()
	if _, _, err := user.NewService(user.NewPostgresStore(pool)).Ensure(ctx, uploader, user.Profile{Email: "direct-once@example.test"}); err != nil {
		t.Fatal(err)
	}
	store := media.NewPostgresStore(pool)
	now := time.Now().UTC()
	rec := directUploadRecord(uploader, now.Add(time.Hour))
	if err := store.StageDirectUpload(ctx, rec); err != nil {
		t.Fatal(err)
	}
	if err := store.DropDirectUpload(ctx, rec.ID, now); err != nil {
		t.Fatal(err)
	}
	if err := store.DropDirectUpload(ctx, rec.ID, now); !errors.Is(err, media.ErrNotFound) {
		t.Fatalf("second drop: %v", err)
	}
	if err := store.StageUpload(ctx, "files/once", uploader, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	_, err := store.PublishDirectUpload(ctx, rec.ID, media.Media{
		Name: rec.Name, Type: "application/pdf", Size: 100, UploadedBy: uploader, Kind: media.KindFile, Key: "files/once",
	}, now)
	if !errors.Is(err, media.ErrNotFound) {
		t.Fatalf("publishing a dropped upload: %v", err)
	}
	// Its pending object is left to the sweeper at once; the other staged
	// object waits for its own time.
	report, err := media.PurgeStagedUploads(ctx, store, media.NewMemoryBlob(), now, 10)
	if err != nil || report.Scanned != 1 || report.Resolved != 1 {
		t.Fatalf("sweep: %+v err %v", report, err)
	}
}
