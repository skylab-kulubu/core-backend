package media_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/lifecycle"
	"github.com/skylab-kulubu/core-backend/internal/media"
)

func TestPurgeDeleted_RemovesOnlyExpiredUnreferencedBlobAndMakesRestoreGone(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	now := time.Date(2026, time.September, 19, 18, 0, 0, 0, time.UTC)
	deletedAt := now.Add(-31 * 24 * time.Hour)
	uploader := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	operator := authz.Principal{ID: uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa").String(), Groups: []string{"/UYELER/YK"}}
	store := media.NewMemoryStore()
	blobs := media.NewMemoryBlob()
	created, err := store.Create(ctx, media.Media{
		Name: "old.png", Type: "image/png", Kind: media.KindImage, Key: "images/old", UploadedBy: uploader,
		DeletedAt: &deletedAt,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := blobs.Put(ctx, created.Key, pngDot(), media.BlobMetadata{ContentType: "image/png"}); err != nil {
		t.Fatal(err)
	}

	report, err := media.PurgeDeleted(ctx, store, blobs, now, 30*24*time.Hour, 25)
	if err != nil {
		t.Fatal(err)
	}
	if report.Scanned != 1 || report.Purged != 1 || report.Referenced != 0 {
		t.Fatalf("report %+v", report)
	}
	if _, ok := blobs.Get(created.Key); ok {
		t.Fatal("expired unreferenced blob remained")
	}
	stored, err := store.GetIncludingDeleted(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.BlobPurgedAt == nil || !stored.BlobPurgedAt.Equal(now) {
		t.Fatalf("purged metadata %+v", stored)
	}

	svc := media.NewService(store, blobs, authz.NewAuthorizer(authz.DefaultPolicy()), "https://cdn.example.test")
	archived, err := svc.ListLifecycle(ctx, operator, lifecycle.InactiveOnly)
	if err != nil {
		t.Fatal(err)
	}
	if len(archived) != 1 || archived[0].URL != "" || archived[0].BlobPurgedAt == nil {
		t.Fatalf("purged lifecycle representation = %+v", archived)
	}
	if _, err := svc.Restore(ctx, operator, created.ID); !errors.Is(err, media.ErrPurged) {
		t.Fatalf("restore after purge = %v, want ErrPurged", err)
	}
	repeat, err := media.PurgeDeleted(ctx, store, blobs, now.Add(time.Hour), 30*24*time.Hour, 25)
	if err != nil {
		t.Fatal(err)
	}
	if repeat.Scanned != 0 || repeat.Purged != 0 {
		t.Fatalf("repeated purge %+v", repeat)
	}
}

func TestPurgeDeleted_DefersRecentAndReferencedMedia(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	now := time.Date(2026, time.September, 19, 18, 0, 0, 0, time.UTC)
	uploader := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	store := media.NewMemoryStore()
	blobs := media.NewMemoryBlob()
	createDeleted := func(key string, deletedAt time.Time) media.Media {
		t.Helper()
		item, err := store.Create(ctx, media.Media{
			Name: key + ".png", Type: "image/png", Kind: media.KindImage, Key: "images/" + key, UploadedBy: uploader,
			DeletedAt: &deletedAt,
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := blobs.Put(ctx, item.Key, pngDot(), media.BlobMetadata{ContentType: item.Type}); err != nil {
			t.Fatal(err)
		}
		return item
	}
	recent := createDeleted("recent", now.Add(-29*24*time.Hour))
	referenced := createDeleted("referenced", now.Add(-31*24*time.Hour))
	store.SetReferenced(referenced.ID, true)

	report, err := media.PurgeDeleted(ctx, store, blobs, now, media.DefaultBlobRecoveryWindow, 25)
	if err != nil {
		t.Fatal(err)
	}
	if report.Scanned != 1 || report.Purged != 0 || report.Referenced != 1 {
		t.Fatalf("report %+v", report)
	}
	for _, item := range []media.Media{recent, referenced} {
		if _, ok := blobs.Get(item.Key); !ok {
			t.Fatalf("protected blob was purged: %s", item.Key)
		}
	}
}

type failingDeleteBlob struct {
	*media.MemoryBlob
	err error
}

func (b failingDeleteBlob) Delete(context.Context, string) error { return b.err }

func TestPurgeDeleted_DoesNotMarkBlobPurgedWhenStorageDeletionFails(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	now := time.Date(2026, time.September, 19, 18, 0, 0, 0, time.UTC)
	deletedAt := now.Add(-31 * 24 * time.Hour)
	store := media.NewMemoryStore()
	base := media.NewMemoryBlob()
	item, err := store.Create(ctx, media.Media{
		Name: "retry.png", Type: "image/png", Kind: media.KindImage, Key: "images/retry",
		UploadedBy: uuid.MustParse("33333333-3333-3333-3333-333333333333"), DeletedAt: &deletedAt,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := base.Put(ctx, item.Key, pngDot(), media.BlobMetadata{ContentType: item.Type}); err != nil {
		t.Fatal(err)
	}
	wantErr := errors.New("r2 unavailable")
	if _, err := media.PurgeDeleted(ctx, store, failingDeleteBlob{MemoryBlob: base, err: wantErr}, now, media.DefaultBlobRecoveryWindow, 25); !errors.Is(err, wantErr) {
		t.Fatalf("purge error = %v", err)
	}
	stored, err := store.GetIncludingDeleted(ctx, item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.BlobPurgedAt != nil {
		t.Fatalf("failed deletion marked purged: %+v", stored)
	}
	if stored.BlobPurgeStartedAt == nil {
		t.Fatalf("failed deletion did not retain its durable claim: %+v", stored)
	}
	svc := media.NewService(store, base, authz.NewAuthorizer(authz.DefaultPolicy()), "")
	operator := authz.Principal{ID: uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa").String(), Groups: []string{"/UYELER/YK"}}
	if _, err := svc.Restore(ctx, operator, item.ID); !errors.Is(err, media.ErrPurgeInProgress) {
		t.Fatalf("restore during purge = %v, want ErrPurgeInProgress", err)
	}
	if _, ok := base.Get(item.Key); !ok {
		t.Fatal("failing storage deletion removed blob")
	}
	retry, err := media.PurgeDeleted(ctx, store, base, now.Add(time.Minute), media.DefaultBlobRecoveryWindow, 25)
	if err != nil {
		t.Fatal(err)
	}
	if retry.Purged != 1 {
		t.Fatalf("retry %+v", retry)
	}
}

func TestBlobPurgeConfigFromEnv(t *testing.T) {
	t.Parallel()
	values := map[string]string{
		"MEDIA_BLOB_RECOVERY_DAYS":    "45",
		"MEDIA_BLOB_PURGE_INTERVAL":   "15m",
		"MEDIA_BLOB_PURGE_BATCH_SIZE": "12",
	}
	config, err := media.BlobPurgeConfigFromEnv(func(key string) string { return values[key] })
	if err != nil {
		t.Fatal(err)
	}
	if config.RecoveryWindow != 45*24*time.Hour || config.Interval != 15*time.Minute || config.BatchSize != 12 {
		t.Fatalf("config %+v", config)
	}
	values["MEDIA_BLOB_PURGE_BATCH_SIZE"] = "0"
	if _, err := media.BlobPurgeConfigFromEnv(func(key string) string { return values[key] }); err == nil {
		t.Fatal("zero batch size must be rejected")
	}
}

func TestUploadStagingConfigFromEnv(t *testing.T) {
	values := map[string]string{
		"MEDIA_UPLOAD_STAGING_GRACE":          "3h",
		"MEDIA_UPLOAD_STAGING_SWEEP_INTERVAL": "5m",
		"MEDIA_UPLOAD_STAGING_BATCH_SIZE":     "12",
	}
	config, err := media.UploadStagingConfigFromEnv(func(key string) string { return values[key] })
	if err != nil {
		t.Fatal(err)
	}
	if config.Grace != 3*time.Hour || config.Interval != 5*time.Minute || config.BatchSize != 12 {
		t.Fatalf("config=%+v", config)
	}
	values["MEDIA_UPLOAD_STAGING_GRACE"] = "1m"
	if _, err := media.UploadStagingConfigFromEnv(func(key string) string { return values[key] }); err == nil {
		t.Fatal("expected too-short staging grace to fail closed")
	}
}

type notifyingBlob struct {
	*media.MemoryBlob
	once    sync.Once
	deleted chan struct{}
}

func (b *notifyingBlob) Delete(ctx context.Context, key string) error {
	err := b.MemoryBlob.Delete(ctx, key)
	b.once.Do(func() { close(b.deleted) })
	return err
}

func TestMaintainBlobPurge_RunsBoundedBatchAndStopsWithContext(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	store := media.NewMemoryStore()
	base := media.NewMemoryBlob()
	blobs := &notifyingBlob{MemoryBlob: base, deleted: make(chan struct{})}
	deletedAt := time.Now().UTC().Add(-48 * time.Hour)
	for i := 0; i < 2; i++ {
		item, err := store.Create(ctx, media.Media{
			Name: "bounded.png", Type: "image/png", Kind: media.KindImage,
			Key: "images/bounded-" + string(rune('a'+i)), UploadedBy: uuid.New(), DeletedAt: &deletedAt,
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := base.Put(ctx, item.Key, pngDot(), media.BlobMetadata{ContentType: item.Type}); err != nil {
			t.Fatal(err)
		}
	}
	done := media.MaintainBlobPurge(ctx, store, blobs, media.BlobPurgeConfig{
		RecoveryWindow: 24 * time.Hour,
		Interval:       time.Hour,
		BatchSize:      1,
	}, func(err error) { t.Errorf("worker: %v", err) })
	select {
	case <-blobs.deleted:
	case <-time.After(2 * time.Second):
		t.Fatal("worker did not run its initial batch")
	}
	items, err := store.ListLifecycle(ctx, lifecycle.All)
	if err != nil {
		t.Fatal(err)
	}
	purged := 0
	for _, item := range items {
		if item.BlobPurgedAt != nil {
			purged++
		}
	}
	if purged != 1 {
		t.Fatalf("purged %d items, want one bounded batch", purged)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("worker did not stop after context cancellation")
	}
}

type contextBlockingBlob struct {
	*media.MemoryBlob
	entered chan struct{}
	once    sync.Once
}

func (b *contextBlockingBlob) Delete(ctx context.Context, _ string) error {
	b.once.Do(func() { close(b.entered) })
	<-ctx.Done()
	return ctx.Err()
}

func TestPurgeDeleted_CancellationBoundsBlobDeletionAndKeepsClaim(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	store := media.NewMemoryStore()
	base := media.NewMemoryBlob()
	deletedAt := time.Now().UTC().Add(-48 * time.Hour)
	item, err := store.Create(ctx, media.Media{
		Name: "blocked.png", Type: "image/png", Kind: media.KindImage, Key: "images/blocked",
		UploadedBy: uuid.New(), DeletedAt: &deletedAt,
	})
	if err != nil {
		t.Fatal(err)
	}
	blobs := &contextBlockingBlob{MemoryBlob: base, entered: make(chan struct{})}
	result := make(chan error, 1)
	go func() {
		_, err := media.PurgeDeleted(ctx, store, blobs, time.Now().UTC(), 24*time.Hour, 1)
		result <- err
	}()
	select {
	case <-blobs.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("blob deletion did not begin")
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("purge error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("purge did not stop after cancellation")
	}
	stored, err := store.GetIncludingDeleted(context.Background(), item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.BlobPurgeStartedAt == nil || stored.BlobPurgedAt != nil {
		t.Fatalf("cancelled purge state %+v", stored)
	}
}
