package account_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/account"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/media"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
	"github.com/skylab-kulubu/core-backend/internal/testpostgres"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

type successfulIdentity struct{}

func (successfulIdentity) EnsureDisabled(context.Context, uuid.UUID) error  { return nil }
func (successfulIdentity) EnsureLoggedOut(context.Context, uuid.UUID) error { return nil }
func (successfulIdentity) EnsureDeleted(context.Context, uuid.UUID) error   { return nil }

type restoreBeforeFirstErase struct {
	store    *media.PostgresStore
	delegate *media.ImmediateBlobEraser
	once     sync.Once
	err      error
}

type deletionRaceBlob struct {
	*media.MemoryBlob
	putStarted chan struct{}
	releasePut chan struct{}
	once       sync.Once
	mu         sync.Mutex
	deleteErr  error
}

func (b *deletionRaceBlob) Put(ctx context.Context, key string, data []byte, contentType string) error {
	b.once.Do(func() { close(b.putStarted) })
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-b.releasePut:
	}
	return b.MemoryBlob.Put(ctx, key, data, contentType)
}

func (b *deletionRaceBlob) Delete(ctx context.Context, key string) error {
	b.mu.Lock()
	err := b.deleteErr
	b.mu.Unlock()
	if err != nil {
		return err
	}
	return b.MemoryBlob.Delete(ctx, key)
}

func (b *deletionRaceBlob) allowDelete() {
	b.mu.Lock()
	b.deleteErr = nil
	b.mu.Unlock()
}

func (e *restoreBeforeFirstErase) EnsureErased(ctx context.Context, id uuid.UUID, at time.Time) error {
	e.once.Do(func() { e.err = e.store.Restore(ctx, id) })
	if e.err != nil {
		return e.err
	}
	return e.delegate.EnsureErased(ctx, id, at)
}

func (e *restoreBeforeFirstErase) EnsureSubjectUploadsErased(ctx context.Context, id uuid.UUID, at time.Time) error {
	return e.delegate.EnsureSubjectUploadsErased(ctx, id, at)
}

func TestWorkerImmediatelyErasesUnreferencedProfileBlobAndSanitizesSharedMedia(t *testing.T) {
	pool := testpostgres.Start(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	users := user.NewPostgresStore(pool)
	mediaStore := media.NewPostgresStore(pool)
	blobs := media.NewMemoryBlob()

	run := func(t *testing.T, reference string) (media.Media, string) {
		t.Helper()
		subjectID := uuid.New()
		if _, _, err := user.NewService(users).Ensure(ctx, subjectID, user.Profile{Email: uuid.NewString() + "@example.test"}); err != nil {
			t.Fatal(err)
		}
		item, err := mediaStore.Create(ctx, media.Media{
			Name: "Yusuf-Acmaci-private-profile.png", Type: "image/png", Kind: media.KindImage,
			Key: "images/" + uuid.NewString(), UploadedBy: subjectID,
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := blobs.Put(ctx, item.Key, []byte("private-profile"), item.Type); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `UPDATE users SET profile_picture_id=$2, profile_picture_url=$3 WHERE id=$1`, subjectID, item.ID, item.Key); err != nil {
			t.Fatal(err)
		}
		switch reference {
		case "event":
			if _, err := event.NewPostgresStore(pool).Create(ctx, event.Event{
				Name: "Shared cover", Location: "YTÜ", OwnerTeam: "WEBLAB", CoverImageID: &item.ID,
			}); err != nil {
				t.Fatal(err)
			}
		case "certificate":
			if _, err := pool.Exec(ctx, `
				INSERT INTO certificate_templates (id, name, owner_team, source_kind, draft_layout, created_by)
				VALUES ($1, 'Shared profile asset', 'WEBLAB', 'upload', jsonb_build_object('backgroundMediaId', $2::text), $3)
			`, uuid.New(), item.ID, subjectID); err != nil {
				t.Fatal(err)
			}
		}
		request, err := users.RequestDeletion(ctx, subjectID, nil)
		if err != nil {
			t.Fatal(err)
		}
		// Anchor the worker clock on the DB-assigned schedule so claims never depend on the calendar.
		now := request.NextAttemptAt
		confirmDeletionProjection(t, users, request, now)
		worker := account.NewWorker(users, successfulIdentity{}, account.WorkerConfig{
			Now: func() time.Time { return now }, Lease: time.Minute, AccessBlocker: &accountBlockWriter{},
		}, media.NewImmediateBlobEraser(mediaStore, blobs))
		if worked, err := worker.RunOnce(ctx); err != nil || !worked {
			t.Fatalf("worker worked=%v err=%v", worked, err)
		}
		if retained, err := users.ProfileMediaForDeletion(ctx, request.ID); err != nil || retained != nil {
			t.Fatalf("completed profile erasure retained association: id=%v err=%v", retained, err)
		}
		stored, err := mediaStore.GetIncludingDeleted(ctx, item.ID)
		if err != nil {
			t.Fatal(err)
		}
		if stored.Name != "" || stored.UploadedBy != uuid.Nil {
			t.Fatalf("profile metadata retained PII: %+v", stored)
		}
		return stored, item.Key
	}

	t.Run("unreferenced", func(t *testing.T) {
		stored, key := run(t, "")
		if stored.DeletedAt == nil || stored.BlobPurgedAt == nil {
			t.Fatalf("unreferenced profile was not immediately purgeable: %+v", stored)
		}
		if _, ok := blobs.Get(key); ok {
			t.Fatal("unreferenced profile blob remained")
		}
	})
	for _, reference := range []string{"event", "certificate"} {
		t.Run("retained-"+reference+"-reference", func(t *testing.T) {
			stored, key := run(t, reference)
			if stored.DeletedAt != nil || stored.BlobPurgedAt != nil {
				t.Fatalf("shared profile media was archived or purged: %+v", stored)
			}
			if _, ok := blobs.Get(key); !ok {
				t.Fatal("shared media blob was removed")
			}
		})
	}
}

func TestWorkerRetriesWhenProfileMediaIsRestoredBeforeBlobErase(t *testing.T) {
	pool := testpostgres.Start(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	users := user.NewPostgresStore(pool)
	mediaStore := media.NewPostgresStore(pool)
	blobs := media.NewMemoryBlob()
	subjectID := uuid.New()
	if _, _, err := user.NewService(users).Ensure(ctx, subjectID, user.Profile{Email: "restore-race@example.test"}); err != nil {
		t.Fatal(err)
	}
	item, err := mediaStore.Create(ctx, media.Media{
		Name: "restore-race.png", Type: "image/png", Kind: media.KindImage,
		Key: "images/" + uuid.NewString(), UploadedBy: subjectID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := blobs.Put(ctx, item.Key, []byte("private-profile"), item.Type); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE users SET profile_picture_id=$2, profile_picture_url=$3 WHERE id=$1`, subjectID, item.ID, item.Key); err != nil {
		t.Fatal(err)
	}
	request, err := users.RequestDeletion(ctx, subjectID, nil)
	if err != nil {
		t.Fatal(err)
	}
	now := request.NextAttemptAt
	confirmDeletionProjection(t, users, request, now)
	eraser := &restoreBeforeFirstErase{
		store:    mediaStore,
		delegate: media.NewImmediateBlobEraser(mediaStore, blobs),
	}
	worker := account.NewWorker(users, successfulIdentity{}, account.WorkerConfig{
		Now: func() time.Time { return now }, Lease: time.Minute, AccessBlocker: &accountBlockWriter{},
	}, eraser)

	worked, err := worker.RunOnce(ctx)
	if !worked || !errors.Is(err, media.ErrProfileBlobNotErased) {
		t.Fatalf("first worker worked=%v err=%v", worked, err)
	}
	var status string
	var retained *uuid.UUID
	var eraseSteps int
	if err := pool.QueryRow(ctx, `
		SELECT status, profile_media_id,
		       (SELECT count(*) FROM account_deletion_steps WHERE request_id=$1 AND step='erase_profile_media')
		FROM account_deletion_requests WHERE id=$1
	`, request.ID).Scan(&status, &retained, &eraseSteps); err != nil {
		t.Fatal(err)
	}
	if status != "pending" || retained == nil || *retained != item.ID || eraseSteps != 0 {
		t.Fatalf("failed erase checkpointed: status=%s retained=%v steps=%d", status, retained, eraseSteps)
	}
	stored, err := mediaStore.GetIncludingDeleted(ctx, item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.DeletedAt != nil || stored.BlobPurgedAt != nil {
		t.Fatalf("race did not leave restored unpurged media: %+v", stored)
	}
	if _, ok := blobs.Get(item.Key); !ok {
		t.Fatal("first erase unexpectedly removed restored blob")
	}

	if err := mediaStore.Archive(ctx, item.ID, nil); err != nil {
		t.Fatal(err)
	}
	if worked, err := worker.RunOnce(ctx); err != nil || !worked {
		t.Fatalf("retry worker worked=%v err=%v", worked, err)
	}
	if err := pool.QueryRow(ctx, `SELECT status, profile_media_id FROM account_deletion_requests WHERE id=$1`, request.ID).Scan(&status, &retained); err != nil {
		t.Fatal(err)
	}
	if status != "completed" || retained != nil {
		t.Fatalf("retry status=%s retained=%v", status, retained)
	}
	stored, err = mediaStore.GetIncludingDeleted(ctx, item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.BlobPurgedAt == nil {
		t.Fatalf("retry did not purge profile blob: %+v", stored)
	}
	if _, ok := blobs.Get(item.Key); ok {
		t.Fatal("profile blob remained after retry")
	}
}

func TestDeletionCannotCompleteWhileJITAuthorizedUploadNeedsDurableCleanup(t *testing.T) {
	pool := testpostgres.Start(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	users := user.NewPostgresStore(pool)
	mediaStore := media.NewPostgresStore(pool)
	subjectID := uuid.New()
	// This Ensure is the same active-account decision made by JIT before the
	// request enters the upload handler.
	if _, _, err := user.NewService(users).Ensure(ctx, subjectID, user.Profile{Email: "jit-upload-race@example.test"}); err != nil {
		t.Fatal(err)
	}
	deleteErr := errors.New("r2 delete unavailable")
	blobs := &deletionRaceBlob{
		MemoryBlob: media.NewMemoryBlob(), putStarted: make(chan struct{}), releasePut: make(chan struct{}), deleteErr: deleteErr,
	}
	svc := media.NewServiceWithOptions(mediaStore, blobs, authz.NewAuthorizer(authz.DefaultPolicy()), "", media.ServiceOptions{
		UploadStagingGrace: 2 * time.Minute,
	})
	uploadDone := make(chan error, 1)
	go func() {
		_, err := svc.Upload(ctx, authz.Principal{ID: subjectID.String()}, "race.txt", "text/plain", []byte("private upload"))
		uploadDone <- err
	}()
	<-blobs.putStarted

	request, err := users.RequestDeletion(ctx, subjectID, nil)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	confirmDeletionProjection(t, users, request, now)
	worker := account.NewWorker(users, successfulIdentity{}, account.WorkerConfig{
		Now: func() time.Time { return now }, Lease: time.Minute, RetryDelay: 0, MaxAttempts: 1,
		AccessBlocker: &accountBlockWriter{},
	}, media.NewImmediateBlobEraser(mediaStore, blobs))
	if worked, err := worker.RunOnce(ctx); !worked || !errors.Is(err, media.ErrStagedUploadNotErased) || !errors.Is(err, media.ErrStagedUploadInFlight) {
		t.Fatalf("worker during put worked=%v err=%v", worked, err)
	}

	close(blobs.releasePut)
	if err := <-uploadDone; !errors.Is(err, media.ErrForbidden) || !errors.Is(err, deleteErr) {
		t.Fatalf("racing upload error = %v", err)
	}
	var key string
	if err := pool.QueryRow(ctx, `SELECT object_key FROM media_upload_staging WHERE subject_id=$1`, subjectID).Scan(&key); err != nil {
		t.Fatal(err)
	}
	if _, ok := blobs.Get(key); !ok {
		t.Fatal("failed compensation did not leave the blob for durable cleanup")
	}
	pending, err := users.DeletionRequest(ctx, subjectID)
	if err != nil || pending.Status != user.DeletionRequestPending {
		t.Fatalf("request exhausted budget while staged upload was leased: %+v err=%v", pending, err)
	}

	now = now.Add(3 * time.Hour)
	if worked, err := worker.RunOnce(ctx); !worked || !errors.Is(err, media.ErrStagedUploadNotErased) || !errors.Is(err, deleteErr) {
		t.Fatalf("worker with R2 failure worked=%v err=%v", worked, err)
	}
	pending, err = users.DeletionRequest(ctx, subjectID)
	if err != nil || pending.Status != user.DeletionRequestPending || !pending.NextAttemptAt.Equal(now.Add(time.Hour)) {
		t.Fatalf("request exhausted budget after durable R2 retry: %+v err=%v", pending, err)
	}

	blobs.allowDelete()
	now = now.Add(2 * time.Hour)
	if worked, err := worker.RunOnce(ctx); !worked || err != nil {
		t.Fatalf("worker after R2 recovery worked=%v err=%v", worked, err)
	}
	completed, err := users.DeletionRequest(ctx, subjectID)
	if err != nil || completed.Status != user.DeletionRequestCompleted {
		t.Fatalf("completed request=%+v err=%v", completed, err)
	}
	var staged int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM media_upload_staging WHERE subject_id=$1`, subjectID).Scan(&staged); err != nil || staged != 0 {
		t.Fatalf("staged rows=%d err=%v", staged, err)
	}
	if _, ok := blobs.Get(key); ok {
		t.Fatal("resolved account cleanup left staged blob")
	}
}
