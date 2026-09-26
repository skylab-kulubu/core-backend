package media_test

import (
	"context"
	"errors"
	"image/color"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/media"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
	"github.com/skylab-kulubu/core-backend/internal/testpostgres"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

func postgresImageStore(t *testing.T) (*media.PostgresStore, uuid.UUID, func(sql string, args ...any)) {
	t.Helper()
	pool := testpostgres.Start(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	uploader := uuid.New()
	if _, _, err := user.NewService(user.NewPostgresStore(pool)).Ensure(ctx, uploader, user.Profile{
		Email: "image-owner@example.com", FirstName: "Image", LastName: "Owner", Username: "image-owner",
	}); err != nil {
		t.Fatal(err)
	}
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	return media.NewPostgresStore(pool), uploader, exec
}

func TestPostgresMediaKeepsItsImageSizes(t *testing.T) {
	store, uploader, _ := postgresImageStore(t)
	ctx := context.Background()
	sizes := map[string]media.SizeObject{
		"card": {ImageSize: media.ImageSize{Width: 400, Height: 300}, Type: "image/jpeg"},
		"page": {ImageSize: media.ImageSize{Width: 1200, Height: 900}, Type: "image/jpeg"},
	}

	photo, err := store.Create(ctx, media.Media{
		Name: "photo.jpg", Type: "image/jpeg", Kind: media.KindImage, Key: "images/photo", UploadedBy: uploader,
		Purpose: "event_cover", Width: 1600, Height: 1200, SizeObjects: sizes,
	})
	if err != nil {
		t.Fatal(err)
	}
	small, err := store.Create(ctx, media.Media{
		Name: "small.png", Type: "image/png", Kind: media.KindImage, Key: "images/small", UploadedBy: uploader,
		Purpose: "event_cover", Width: 300, Height: 200, SizeObjects: map[string]media.SizeObject{},
	})
	if err != nil {
		t.Fatal(err)
	}
	notes, err := store.Create(ctx, media.Media{Name: "notes.pdf", Type: "application/pdf", Kind: media.KindFile, Key: "files/notes", UploadedBy: uploader})
	if err != nil {
		t.Fatal(err)
	}

	got, err := store.Get(ctx, photo.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Width != 1600 || got.Height != 1200 || !reflect.DeepEqual(got.SizeObjects, sizes) {
		t.Fatalf("photo %d×%d %v", got.Width, got.Height, got.SizeObjects)
	}
	got, err = store.Get(ctx, small.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.SizeObjects == nil || len(got.SizeObjects) != 0 {
		t.Fatalf("an image with no stored size reads as %v, want empty (made, none needed)", got.SizeObjects)
	}
	got, err = store.Get(ctx, notes.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Width != 0 || got.Height != 0 || got.SizeObjects != nil {
		t.Fatalf("a PDF reads with %d×%d %v", got.Width, got.Height, got.SizeObjects)
	}
}

func TestPostgresListsTheImagesWaitingForTheirSizes(t *testing.T) {
	store, uploader, exec := postgresImageStore(t)
	ctx := context.Background()
	image := func(key string) media.Media {
		t.Helper()
		created, err := store.Create(ctx, media.Media{Name: key, Type: "image/png", Kind: media.KindImage, Key: key, UploadedBy: uploader, Purpose: "event_cover"})
		if err != nil {
			t.Fatal(err)
		}
		return created
	}
	waiting := image("images/waiting")
	done, err := store.Create(ctx, media.Media{
		Name: "done", Type: "image/png", Kind: media.KindImage, Key: "images/done", UploadedBy: uploader, Purpose: "event_cover", SizeObjects: map[string]media.SizeObject{},
	})
	if err != nil {
		t.Fatal(err)
	}
	archived := image("images/archived")
	if err := store.Archive(ctx, archived.ID, nil); err != nil {
		t.Fatal(err)
	}
	purging := image("images/purging")
	exec(`UPDATE media SET blob_purge_started_at = now() WHERE id = $1`, purging.ID)
	if _, err := store.Create(ctx, media.Media{Name: "notes.pdf", Type: "application/pdf", Kind: media.KindFile, Key: "files/notes", UploadedBy: uploader}); err != nil {
		t.Fatal(err)
	}
	// Uploaded without a purpose: not listed for the purposes that get sizes.
	if _, err := store.Create(ctx, media.Media{Name: "legacy", Type: "image/png", Kind: media.KindImage, Key: "images/legacy", UploadedBy: uploader}); err != nil {
		t.Fatal(err)
	}
	purposes := []string{"event_cover", "profile_picture"}

	pending, err := store.ListPendingImageSizes(ctx, purposes, uuid.Nil, 25)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].ID != waiting.ID {
		t.Fatalf("pending %v, want only %s", pending, waiting.ID)
	}
	if after, err := store.ListPendingImageSizes(ctx, purposes, waiting.ID, 25); err != nil || len(after) != 0 {
		t.Fatalf("after the last one: %v %v", after, err)
	}

	sizes := map[string]media.SizeObject{"card": {ImageSize: media.ImageSize{Width: 400, Height: 200}, Type: "image/png"}}
	if err := store.SetImageSizes(ctx, waiting.ID, media.ImageSize{Width: 800, Height: 400}, sizes); err != nil {
		t.Fatal(err)
	}
	got, err := store.Get(ctx, waiting.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Width != 800 || got.Height != 400 || !reflect.DeepEqual(got.SizeObjects, sizes) {
		t.Fatalf("recorded %d×%d %v", got.Width, got.Height, got.SizeObjects)
	}
	if pending, _ := store.ListPendingImageSizes(ctx, purposes, uuid.Nil, 25); len(pending) != 0 {
		t.Fatalf("still pending %v", pending)
	}
	// Nil puts it back to not made: a write that failed is retried.
	if err := store.SetImageSizes(ctx, waiting.ID, media.ImageSize{}, nil); err != nil {
		t.Fatal(err)
	}
	if pending, _ := store.ListPendingImageSizes(ctx, purposes, uuid.Nil, 25); len(pending) != 1 {
		t.Fatalf("pending again %v", pending)
	}
	// An image that does not decode is recorded with no size and no stored
	// sizes, so it is not listed again.
	undecodable := image("images/undecodable")
	if err := store.SetImageSizes(ctx, undecodable.ID, media.ImageSize{}, map[string]media.SizeObject{}); err != nil {
		t.Fatal(err)
	}
	if got, _ := store.Get(ctx, undecodable.ID); got.Width != 0 || got.SizeObjects == nil {
		t.Fatalf("undecodable recorded as %d %v", got.Width, got.SizeObjects)
	}

	if err := store.SetImageSizes(ctx, purging.ID, media.ImageSize{Width: 1, Height: 1}, sizes); !errors.Is(err, media.ErrPurgeInProgress) {
		t.Fatalf("recording sizes of an image being purged: %v, want %v", err, media.ErrPurgeInProgress)
	}
	if err := store.SetImageSizes(ctx, done.ID, media.ImageSize{Width: 5, Height: 5}, map[string]media.SizeObject{}); err != nil {
		t.Fatalf("recording again: %v", err)
	}
}

func TestPostgresPurgeAndStagingSweeperRemoveAnImagesStoredSizes(t *testing.T) {
	store, uploader, exec := postgresImageStore(t)
	ctx := context.Background()
	blobs := media.NewMemoryBlob()
	put := func(key string) {
		t.Helper()
		for _, k := range []string{key, key + "/card.jpg", key + "/page.jpg", key + "/card.png", key + "/page.png"} {
			if err := blobs.Put(ctx, k, pngDot(), media.BlobMetadata{ContentType: "image/png"}); err != nil {
				t.Fatal(err)
			}
		}
	}
	now := time.Now().UTC()

	// An archived image past its recovery window.
	archived, err := store.Create(ctx, media.Media{
		Name: "old.png", Type: "image/png", Kind: media.KindImage, Key: "images/archived-sizes", UploadedBy: uploader,
		SizeObjects: map[string]media.SizeObject{
			"card": {ImageSize: media.ImageSize{Width: 400, Height: 300}, Type: "image/jpeg"},
			"page": {ImageSize: media.ImageSize{Width: 1200, Height: 900}, Type: "image/png"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	put(archived.Key)
	exec(`UPDATE media SET deleted_at = $2 WHERE id = $1`, archived.ID, now.Add(-31*24*time.Hour))
	if report, err := media.PurgeDeleted(ctx, store, blobs, now, 30*24*time.Hour, 25); err != nil || report.Purged != 1 {
		t.Fatalf("purge %+v %v", report, err)
	}

	// An upload that wrote its image and sizes and never published.
	unpublished := "images/unpublished-sizes"
	put(unpublished)
	if err := store.StageUpload(ctx, unpublished, uploader, now.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if report, err := media.PurgeStagedUploads(ctx, store, blobs, now, 25); err != nil || report.Resolved != 1 {
		t.Fatalf("sweep %+v %v", report, err)
	}

	if left := blobs.Keys(); len(left) != 0 {
		t.Fatalf("objects left: %v", left)
	}
}

func TestPostgresImageSizeBackfillMakesTheSizesOfARowStoredBefore(t *testing.T) {
	store, uploader, exec := postgresImageStore(t)
	ctx := context.Background()
	blobs := media.NewMemoryBlob()
	catalogue, err := media.LoadCatalogue()
	if err != nil {
		t.Fatal(err)
	}
	// A purposed row as core stored it before image sizes existed.
	id := uuid.New()
	exec(`INSERT INTO media (id, file_name, file_type, file_url, file_size, uploaded_by, kind, purpose)
		VALUES ($1, 'wide.png', 'image/png', 'images/wide-before', 1, $2, 'IMAGE', 'event_cover')`, id, uploader)
	if err := blobs.Put(ctx, "images/wide-before", solidPNG(t, 1000, 500, color.RGBA{R: 200, A: 255}), media.BlobMetadata{ContentType: "image/png"}); err != nil {
		t.Fatal(err)
	}

	report, err := media.BackfillImageSizes(ctx, store, blobs, catalogue, media.NewDecodeBudget(media.DecodeBudgetConfig{}), nil)
	if err != nil || report != (media.BackfillReport{Applied: 1}) {
		t.Fatalf("report %+v %v", report, err)
	}
	got, err := store.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]media.SizeObject{"card": {ImageSize: media.ImageSize{Width: 400, Height: 200}, Type: "image/png"}}
	if got.Width != 1000 || got.Height != 500 || !reflect.DeepEqual(got.SizeObjects, want) {
		t.Fatalf("recorded %d×%d %v", got.Width, got.Height, got.SizeObjects)
	}
	if _, ok := blobs.Get("images/wide-before/card.png"); !ok {
		t.Fatal("card not stored")
	}
	if again, err := media.BackfillImageSizes(ctx, store, blobs, catalogue, media.NewDecodeBudget(media.DecodeBudgetConfig{}), nil); err != nil || again != (media.BackfillReport{}) {
		t.Fatalf("second pass %+v %v", again, err)
	}
}
