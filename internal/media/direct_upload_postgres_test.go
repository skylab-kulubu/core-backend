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
	if err := store.StageDirectUpload(ctx, rec, 0, now); err != nil {
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
	if err := store.StageDirectUpload(ctx, directUploadRecord(uploader, now.Add(time.Hour)), 0, now); !errors.Is(err, media.ErrForbidden) {
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

// An upload ended is gone for every later end, and a person has at most
// maxOpen uploads open: expired ones do not count.
func TestDirectUploadEndsOnceAndOpenOnesAreCounted(t *testing.T) {
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
	if err := store.StageDirectUpload(ctx, rec, 2, now); err != nil {
		t.Fatal(err)
	}
	if owned, err := store.EndDirectUpload(ctx, rec.ID, uuid.Nil, "", now, now); err != nil || !owned {
		t.Fatalf("end: owned %v err %v", owned, err)
	}
	if owned, err := store.EndDirectUpload(ctx, rec.ID, uuid.Nil, "", now, now); err != nil || owned {
		t.Fatalf("ending it again: owned %v err %v", owned, err)
	}
	if _, err := store.ClaimDirectUpload(ctx, rec.ID, uploader, "files/"+uuid.NewString(), now, now.Add(media.DirectUploadClaimLease)); !errors.Is(err, media.ErrNotFound) {
		t.Fatalf("completing an ended upload: %v", err)
	}
	// Its pending object is left to the sweeper at once.
	report, err := media.PurgeStagedUploads(ctx, store, media.NewMemoryBlob(), now, 10)
	if err != nil || report.Scanned != 1 || report.Resolved != 1 {
		t.Fatalf("sweep: %+v err %v", report, err)
	}

	first := directUploadRecord(uploader, now.Add(time.Hour))
	second := directUploadRecord(uploader, now.Add(2*time.Hour))
	for _, open := range []media.DirectUploadRecord{first, second} {
		if err := store.StageDirectUpload(ctx, open, 2, now); err != nil {
			t.Fatal(err)
		}
	}
	var tooMany *media.TooManyOpenDirectUploads
	err = store.StageDirectUpload(ctx, directUploadRecord(uploader, now.Add(3*time.Hour)), 2, now)
	if !errors.As(err, &tooMany) || !tooMany.NextExpiry.Equal(first.ExpiresAt.Truncate(time.Microsecond)) {
		t.Fatalf("a third open upload: %v", err)
	}
	if err := store.StageDirectUpload(ctx, directUploadRecord(uploader, now.Add(3*time.Hour)), 2, first.ExpiresAt); err != nil {
		t.Fatalf("once the first expired: %v", err)
	}
}

// A completion claims its upload in a short transaction and holds no lock
// while storage works. A live claim turns other completions away; one whose
// lease ran out is stale: the sweeper deletes what it left, the pending
// object and the upload at the lease's end, the copy once a late copy can
// no longer land.
func TestDirectUploadClaimsLeaseTheirUpload(t *testing.T) {
	pool := testpostgres.Start(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	uploader := uuid.New()
	if _, _, err := user.NewService(user.NewPostgresStore(pool)).Ensure(ctx, uploader, user.Profile{Email: "direct-claim@example.test"}); err != nil {
		t.Fatal(err)
	}
	store := media.NewPostgresStore(pool)
	r2, fake := multipartR2(t)
	now := time.Now().UTC()
	stage := func() media.DirectUploadRecord {
		t.Helper()
		rec := directUploadRecord(uploader, now.Add(media.DirectUploadTTL))
		if err := store.StageDirectUpload(ctx, rec, 0, now); err != nil {
			t.Fatal(err)
		}
		if err := store.SetDirectUploadMultipart(ctx, rec.ID, "mp-"+rec.ID.String()); err != nil {
			t.Fatal(err)
		}
		return rec
	}

	// Released: back to open, with nothing of the claim left.
	rec := stage()
	claim, err := store.ClaimDirectUpload(ctx, rec.ID, uploader, "files/"+uuid.NewString(), now, now.Add(media.DirectUploadClaimLease))
	if err != nil || claim.Upload.ID != rec.ID || claim.FinalKey == "" || !claim.Until.Equal(now.Add(media.DirectUploadClaimLease)) {
		t.Fatalf("claim %+v err %v", claim, err)
	}
	if _, err := store.ClaimDirectUpload(ctx, rec.ID, uploader, "files/"+uuid.NewString(), now.Add(time.Minute), now.Add(time.Hour)); !errors.Is(err, media.ErrDirectUploadCompleting) {
		t.Fatalf("a second claim under a live lease: %v", err)
	}
	if _, err := store.ClaimDirectUpload(ctx, rec.ID, uuid.New(), "files/"+uuid.NewString(), now, now.Add(time.Hour)); !errors.Is(err, media.ErrNotFound) {
		t.Fatalf("someone else's claim: %v", err)
	}
	if err := store.ReleaseDirectUpload(ctx, claim); err != nil {
		t.Fatal(err)
	}
	var staged int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM media_upload_staging WHERE object_key=$1`, claim.FinalKey).Scan(&staged); err != nil || staged != 0 {
		t.Fatalf("the released claim's final key is still staged (%d, %v)", staged, err)
	}
	if again, err := store.ClaimDirectUpload(ctx, rec.ID, uploader, "files/"+uuid.NewString(), now, now.Add(media.DirectUploadClaimLease)); err != nil {
		t.Fatalf("claiming a released upload: %v", err)
	} else if err := store.ReleaseDirectUpload(ctx, again); err != nil {
		t.Fatal(err)
	}

	// Stale: the completion died after joining and copying.
	stale := stage()
	claim, err = store.ClaimDirectUpload(ctx, stale.ID, uploader, "files/"+uuid.NewString(), now, now.Add(media.DirectUploadClaimLease))
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{stale.Key, claim.FinalKey} {
		if err := r2.Put(ctx, key, []byte("%PDF-1.7"), media.BlobMetadata{ContentType: "application/octet-stream"}); err != nil {
			t.Fatal(err)
		}
	}
	report, err := media.PurgeStagedUploads(ctx, store, r2, claim.Until.Add(-time.Second), 10)
	if err != nil || report.Scanned != 0 {
		t.Fatalf("the sweeper under a live lease: %+v %v", report, err)
	}
	if _, err := store.FinishDirectUpload(ctx, claim, media.Media{
		Name: stale.Name, Type: "application/pdf", Size: stale.Size, UploadedBy: uploader, Kind: media.KindFile,
		Purpose: stale.Purpose, Visibility: media.VisibilityPublic, Key: claim.FinalKey,
	}, claim.Until); !errors.Is(err, media.ErrDirectUploadClaimLost) {
		t.Fatalf("finishing after the lease: %v", err)
	}
	report, err = media.PurgeStagedUploads(ctx, store, r2, claim.Until, 10)
	if err != nil || report.Resolved != 1 {
		t.Fatalf("the sweeper after the lease: %+v %v", report, err)
	}
	if _, err := store.GetDirectUpload(ctx, stale.ID); !errors.Is(err, media.ErrNotFound) {
		t.Fatalf("the stale upload is still there: %v", err)
	}
	if _, ok := fake.Object("media", stale.Key); ok {
		t.Fatal("the stale pending object is left")
	}
	report, err = media.PurgeStagedUploads(ctx, store, r2, claim.Until.Add(media.DirectUploadLateCopyMargin), 10)
	if err != nil || report.Resolved != 1 {
		t.Fatalf("the sweeper once no late copy can land: %+v %v", report, err)
	}
	if _, ok := fake.Object("media", claim.FinalKey); ok {
		t.Fatal("the stale copy is left")
	}
	// The released upload is still open.
	if _, err := store.GetDirectUpload(ctx, rec.ID); err != nil {
		t.Fatalf("the released upload: %v", err)
	}
}
