package media_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/media"
)

// The in-memory store models the expiry cleanup for fast tests: only an
// expired Media nothing keeps is purged, and a failed deletion is retried.
func TestMemoryStoreExpiryCleanup(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := media.NewMemoryStore()
	blobs := media.NewMemoryBlob()
	now := time.Date(2026, time.September, 26, 12, 0, 0, 0, time.UTC)
	past, future := now.Add(-time.Minute), now.Add(time.Hour)
	create := func(name string, status media.Status, expires *time.Time, archived bool) media.Media {
		t.Helper()
		m := media.Media{
			Name: name, Type: "image/png", Kind: media.KindImage, Key: "images/" + name,
			UploadedBy: uuid.New(), Purpose: "event_gallery", Status: status, ExpiresAt: expires,
		}
		if archived {
			m.DeletedAt = &past
		}
		created, err := store.Create(ctx, m)
		if err != nil {
			t.Fatal(err)
		}
		if err := blobs.Put(ctx, created.Key, pngDot(), media.BlobMetadata{ContentType: created.Type}); err != nil {
			t.Fatal(err)
		}
		return created
	}
	abandoned := create("abandoned", media.StatusPending, &past, false)
	detached := create("detached", media.StatusDetached, &past, false)
	used := create("used", media.StatusDetached, &past, false)
	store.SetReferenced(used.ID, true)
	kept := []media.Media{
		create("attached", media.StatusAttached, nil, false),
		create("legacy", media.StatusPending, nil, false),
		create("fresh", media.StatusPending, &future, false),
		create("archived", media.StatusPending, &past, true),
		used,
	}

	failing := failingKeyBlob{MemoryBlob: blobs, key: detached.Key}
	report, err := media.PurgeExpired(ctx, store, failing, now, nil)
	if err != nil || report.Purged != 1 || report.Kept != 1 || report.Failed != 1 {
		t.Fatalf("report %+v err %v", report, err)
	}
	report, err = media.PurgeExpired(ctx, store, blobs, now, nil)
	if err != nil || report.Purged != 1 || report.Kept != 1 || report.Failed != 0 {
		t.Fatalf("retry report %+v err %v", report, err)
	}
	for _, item := range []media.Media{abandoned, detached} {
		got, err := store.GetIncludingDeleted(ctx, item.ID)
		if err != nil {
			t.Fatal(err)
		}
		if _, blobKept := blobs.Get(item.Key); blobKept || got.BlobPurgedAt == nil || got.DeletedAt == nil {
			t.Fatalf("%s: blob kept %v, record %+v", item.Name, blobKept, got)
		}
	}
	for _, item := range kept {
		if _, ok := blobs.Get(item.Key); !ok {
			t.Errorf("%s: blob purged", item.Name)
		}
	}
}
