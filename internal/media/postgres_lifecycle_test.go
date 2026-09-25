package media_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/media"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
	"github.com/skylab-kulubu/core-backend/internal/testpostgres"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

func TestPostgresBlobPurgeProtectsRetainedDomainReferences(t *testing.T) {
	pool := testpostgres.Start(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	uploader := uuid.New()
	if _, _, err := user.NewService(user.NewPostgresStore(pool)).Ensure(ctx, uploader, user.Profile{
		Email: "media-owner@example.com", FirstName: "Media", LastName: "Owner", Username: "media-owner",
	}); err != nil {
		t.Fatal(err)
	}
	store := media.NewPostgresStore(pool)
	blobs := media.NewMemoryBlob()
	create := func(name string) media.Media {
		t.Helper()
		item, err := store.Create(ctx, media.Media{
			Name: name + ".png", Type: "image/png", Kind: media.KindImage, Key: "images/" + name, UploadedBy: uploader,
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := blobs.Put(ctx, item.Key, pngDot(), media.BlobMetadata{ContentType: item.Type}); err != nil {
			t.Fatal(err)
		}
		return item
	}
	cover := create("cover")
	gallery := create("gallery")
	profile := create("profile")
	template := create("template")
	unattached := create("unattached")

	events := event.NewPostgresStore(pool)
	eventItem, err := events.Create(ctx, event.Event{
		Name: "Retained event", Location: "YTÜ", OwnerTeam: "WEBLAB", CoverImageID: &cover.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := events.AddImages(ctx, eventItem.ID, []uuid.UUID{gallery.ID}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE events SET archived_at = now() WHERE id = $1`, eventItem.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE users SET profile_picture_id = $2, profile_picture_url = $3 WHERE id = $1`, uploader, profile.ID, profile.Key); err != nil {
		t.Fatal(err)
	}
	templateID := uuid.New()
	if _, err := pool.Exec(ctx, `
		INSERT INTO certificate_templates (id, name, owner_team, source_kind, draft_layout, archived_at, created_by)
		VALUES ($1, 'Retained template', 'WEBLAB', 'upload', jsonb_build_object('backgroundMediaId', $2::text), now(), $3)`,
		templateID, template.ID, uploader); err != nil {
		t.Fatal(err)
	}

	deletedAt := time.Now().UTC().Add(-31 * 24 * time.Hour)
	for _, item := range []media.Media{cover, gallery, profile, template, unattached} {
		if err := store.Archive(ctx, item.ID, &uploader); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `UPDATE media SET deleted_at = $2 WHERE id = $1`, item.ID, deletedAt); err != nil {
			t.Fatal(err)
		}
	}

	now := time.Now().UTC()
	report, err := media.PurgeDeleted(ctx, store, blobs, now, media.DefaultBlobRecoveryWindow, 25)
	if err != nil {
		t.Fatal(err)
	}
	if report.Scanned != 5 || report.Purged != 1 || report.Referenced != 4 {
		t.Fatalf("report %+v", report)
	}
	if _, ok := blobs.Get(unattached.Key); ok {
		t.Fatal("unattached blob remained")
	}
	for name, item := range map[string]media.Media{"cover": cover, "gallery": gallery, "profile": profile, "template": template} {
		if _, ok := blobs.Get(item.Key); !ok {
			t.Fatalf("%s blob was purged while retained", name)
		}
		stored, err := store.GetIncludingDeleted(ctx, item.ID)
		if err != nil || stored.BlobPurgedAt != nil {
			t.Fatalf("%s metadata = %+v, err = %v", name, stored, err)
		}
	}
	stored, err := store.GetIncludingDeleted(ctx, unattached.ID)
	if err != nil || stored.BlobPurgedAt == nil {
		t.Fatalf("unattached metadata = %+v, err = %v", stored, err)
	}

	// A later cleanup run can remove the bytes only after every retained owner
	// has explicitly detached the media.
	if _, err := pool.Exec(ctx, `UPDATE events SET cover_image_id = NULL WHERE id = $1`, eventItem.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM event_images WHERE event_id = $1`, eventItem.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE users SET profile_picture_id = NULL, profile_picture_url = '' WHERE id = $1`, uploader); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE certificate_templates SET draft_layout = '{}'::jsonb WHERE id = $1`, templateID); err != nil {
		t.Fatal(err)
	}
	report, err = media.PurgeDeleted(ctx, store, blobs, now.Add(time.Minute), media.DefaultBlobRecoveryWindow, 25)
	if err != nil {
		t.Fatal(err)
	}
	if report.Scanned != 4 || report.Purged != 4 || report.Referenced != 0 {
		t.Fatalf("detached report %+v", report)
	}
	for _, item := range []media.Media{cover, gallery, profile, template} {
		if _, ok := blobs.Get(item.Key); ok {
			t.Fatalf("detached blob remained: %s", item.Key)
		}
	}

	crashed := create("crash-retry")
	if err := store.Archive(ctx, crashed.ID, &uploader); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE media SET deleted_at = $2 WHERE id = $1`, crashed.ID, deletedAt); err != nil {
		t.Fatal(err)
	}
	crashErr := errors.New("process stopped after object deletion")
	if _, err := media.PurgeDeleted(ctx, store, deleteThenErrorBlob{MemoryBlob: blobs, err: crashErr}, now.Add(2*time.Minute), media.DefaultBlobRecoveryWindow, 25); !errors.Is(err, crashErr) {
		t.Fatalf("crash simulation error = %v", err)
	}
	crashState, err := store.GetIncludingDeleted(ctx, crashed.ID)
	if err != nil {
		t.Fatal(err)
	}
	if crashState.BlobPurgeStartedAt == nil || crashState.BlobPurgedAt != nil {
		t.Fatalf("crash state %+v", crashState)
	}
	if _, ok := blobs.Get(crashed.Key); ok {
		t.Fatal("crash simulation did not remove object")
	}
	if err := store.Restore(ctx, crashed.ID); !errors.Is(err, media.ErrPurgeInProgress) {
		t.Fatalf("restore after interrupted purge = %v", err)
	}
	retry, err := media.PurgeDeleted(ctx, store, blobs, now.Add(3*time.Minute), media.DefaultBlobRecoveryWindow, 25)
	if err != nil || retry.Purged != 1 {
		t.Fatalf("crash retry = %+v, err = %v", retry, err)
	}
	if err := store.Restore(ctx, crashed.ID); !errors.Is(err, media.ErrPurged) {
		t.Fatalf("restore after completed retry = %v", err)
	}

	raceItem := create("reference-race")
	if err := store.Archive(ctx, raceItem.ID, &uploader); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE media SET deleted_at = $2 WHERE id = $1`, raceItem.ID, deletedAt); err != nil {
		t.Fatal(err)
	}
	blocked := &blockingDeleteBlob{MemoryBlob: blobs, entered: make(chan struct{}), release: make(chan struct{})}
	purgeResult := make(chan error, 1)
	go func() {
		_, err := media.PurgeDeleted(ctx, store, blocked, now.Add(4*time.Minute), media.DefaultBlobRecoveryWindow, 25)
		purgeResult <- err
	}()
	select {
	case <-blocked.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("purge did not reach object deletion")
	}
	insertResult := make(chan error, 1)
	go func() {
		_, err := pool.Exec(ctx, `INSERT INTO events (id, name, location, owner_team, cover_image_id) VALUES ($1, 'Race', 'YTÜ', 'WEBLAB', $2)`, uuid.New(), raceItem.ID)
		insertResult <- err
	}()
	restoreResult := make(chan error, 1)
	go func() { restoreResult <- store.Restore(ctx, raceItem.ID) }()
	time.Sleep(100 * time.Millisecond)
	var insertErr error
	insertFinished := false
	select {
	case insertErr = <-insertResult:
		insertFinished = true
	default:
	}
	var restoreErr error
	restoreFinished := false
	select {
	case restoreErr = <-restoreResult:
		restoreFinished = true
		if !errors.Is(restoreErr, media.ErrPurgeInProgress) {
			t.Errorf("restore while durable purge claim exists = %v", restoreErr)
		}
	default:
	}
	close(blocked.release)
	if err := <-purgeResult; err != nil {
		t.Fatal(err)
	}
	if !insertFinished {
		insertErr = <-insertResult
	}
	if insertErr == nil {
		t.Fatal("concurrent retained reference to purged media was accepted")
	}
	if !restoreFinished {
		restoreErr = <-restoreResult
	}
	if !errors.Is(restoreErr, media.ErrPurgeInProgress) && !errors.Is(restoreErr, media.ErrPurged) {
		t.Fatalf("concurrent restore = %v", restoreErr)
	}
	if err := store.Restore(ctx, raceItem.ID); !errors.Is(err, media.ErrPurged) {
		t.Fatalf("restore after race purge = %v, want ErrPurged", err)
	}
}

type deleteThenErrorBlob struct {
	*media.MemoryBlob
	err error
}

func (b deleteThenErrorBlob) Delete(ctx context.Context, key string) error {
	if err := b.MemoryBlob.Delete(ctx, key); err != nil {
		return err
	}
	return b.err
}

type blockingDeleteBlob struct {
	*media.MemoryBlob
	once    sync.Once
	entered chan struct{}
	release chan struct{}
}

func (b *blockingDeleteBlob) Delete(ctx context.Context, key string) error {
	b.once.Do(func() { close(b.entered) })
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-b.release:
		return b.MemoryBlob.Delete(ctx, key)
	}
}
