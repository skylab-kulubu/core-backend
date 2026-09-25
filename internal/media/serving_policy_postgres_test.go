package media_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/media"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
	"github.com/skylab-kulubu/core-backend/internal/testpostgres"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

func TestPostgresServingPolicyBackfillRewritesOnlyLegacyObjects(t *testing.T) {
	pool := testpostgres.Start(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	uploader := uuid.New()
	if _, _, err := user.NewService(user.NewPostgresStore(pool)).Ensure(ctx, uploader, user.Profile{
		Email: "serving-owner@example.com", FirstName: "Serving", LastName: "Owner", Username: "serving-owner",
	}); err != nil {
		t.Fatal(err)
	}
	store := media.NewPostgresStore(pool)
	blobs := media.NewMemoryBlob()
	legacy := func(name, ctype, kind, key string) media.Media {
		t.Helper()
		item, err := store.Create(ctx, media.Media{Name: name, Type: ctype, Kind: kind, Key: key, UploadedBy: uploader})
		if err != nil {
			t.Fatal(err)
		}
		if err := blobs.Put(ctx, key, []byte("legacy"), media.BlobMetadata{ContentType: ctype}); err != nil {
			t.Fatal(err)
		}
		return item
	}
	page := legacy("page.html", "text/html", media.KindFile, "files/page")
	archived := legacy("old.html", "text/html", media.KindFile, "files/old")
	if err := store.Archive(ctx, archived.ID, nil); err != nil {
		t.Fatal(err)
	}
	archived, err := store.GetIncludingDeleted(ctx, archived.ID)
	if err != nil {
		t.Fatal(err)
	}
	purged := legacy("gone.html", "text/html", media.KindFile, "files/gone")
	if _, err := pool.Exec(ctx, `UPDATE media SET deleted_at = now(), blob_purge_started_at = now(), blob_purged_at = now() WHERE id = $1`, purged.ID); err != nil {
		t.Fatal(err)
	}
	svc := media.NewService(store, blobs, authz.NewAuthorizer(authz.DefaultPolicy()), "")
	if _, err := svc.Upload(ctx, authz.Principal{ID: uploader.String()}, "notes.txt", "text/plain", []byte("notes")); err != nil {
		t.Fatal(err)
	}

	report, err := media.BackfillServingPolicy(ctx, store, blobs, nil)
	if err != nil || report != (media.BackfillReport{Applied: 2}) {
		t.Fatalf("report %+v err %v", report, err)
	}
	for _, item := range []media.Media{page, archived} {
		stored, err := store.GetIncludingDeleted(ctx, item.ID)
		if err != nil || stored.Type != "text/html" || !stored.UpdatedAt.Equal(item.UpdatedAt) {
			t.Fatalf("%s record type %q updated %v (was %v) err %v", item.Name, stored.Type, stored.UpdatedAt, item.UpdatedAt, err)
		}
		if got, _ := blobs.Metadata(item.Key); got.ContentDisposition != "attachment; filename="+item.Name {
			t.Fatalf("%s metadata %+v", item.Name, got)
		}
	}
	if stored, _ := store.GetIncludingDeleted(ctx, purged.ID); stored.Type != "text/html" {
		t.Fatalf("purged record changed to %q", stored.Type)
	}

	report, err = media.BackfillServingPolicy(ctx, store, blobs, nil)
	if err != nil || report != (media.BackfillReport{}) {
		t.Fatalf("second report %+v err %v", report, err)
	}
}
