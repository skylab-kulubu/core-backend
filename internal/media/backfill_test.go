package media_test

import (
	"context"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/media"
)

func TestBackfillCoverColorsProcessesExistingImages(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := media.NewMemoryStore()
	blobs := media.NewMemoryBlob()
	key := "images/existing"
	if err := blobs.Put(ctx, key, twoTonePNG(t), media.BlobMetadata{ContentType: "image/png"}); err != nil {
		t.Fatal(err)
	}
	existing, err := store.Create(ctx, media.Media{
		ID:         uuid.New(),
		Name:       "existing.png",
		Type:       "image/png",
		Kind:       media.KindImage,
		Key:        key,
		UploadedBy: uuid.New(),
	})
	if err != nil {
		t.Fatal(err)
	}

	processed, done, err := media.BackfillCoverColors(ctx, store, blobs)
	if err != nil {
		t.Fatal(err)
	}
	if processed != 1 || !done {
		t.Fatalf("processed %d done %v", processed, done)
	}
	got, err := store.Get(ctx, existing.ID)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"#3c82be", "#8a642f"}
	if !got.CoverColorsComputed || !reflect.DeepEqual(got.CoverColors, want) {
		t.Fatalf("got %#v computed %v", got.CoverColors, got.CoverColorsComputed)
	}

	processed, done, err = media.BackfillCoverColors(ctx, store, blobs)
	if err != nil || processed != 0 || !done {
		t.Fatalf("second processed %d done %v err %v", processed, done, err)
	}
}

// legacyMedia stores an object the way Upload did before the serving policy:
// with the recorded type as its only metadata.
func legacyMedia(t *testing.T, store *media.MemoryStore, blobs *media.MemoryBlob, m media.Media) media.Media {
	t.Helper()
	ctx := context.Background()
	if err := blobs.Put(ctx, m.Key, []byte("legacy"), media.BlobMetadata{ContentType: m.Type}); err != nil {
		t.Fatal(err)
	}
	if m.UploadedBy == uuid.Nil {
		m.UploadedBy = uuid.New()
	}
	created, err := store.Create(ctx, m)
	if err != nil {
		t.Fatal(err)
	}
	return created
}

func TestBackfillServingPolicyRewritesLegacyDownloadsAndSVGs(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := media.NewMemoryStore()
	blobs := media.NewMemoryBlob()
	page := legacyMedia(t, store, blobs, media.Media{Name: "page.html", Type: "text/html", Kind: media.KindFile, Key: "files/page"})
	logo := legacyMedia(t, store, blobs, media.Media{Name: "logo.svg", Type: "image/svg+xml", Kind: media.KindImage, Key: "images/logo"})

	processed, done, err := media.BackfillServingPolicy(ctx, store, blobs)
	if err != nil || processed != 2 || !done {
		t.Fatalf("processed %d done %v err %v", processed, done, err)
	}
	if got, _ := blobs.Metadata(page.Key); got != (media.BlobMetadata{ContentType: "application/octet-stream", ContentDisposition: "attachment; filename=page.html"}) {
		t.Fatalf("page metadata %+v", got)
	}
	if got, _ := blobs.Metadata(logo.Key); got != (media.BlobMetadata{ContentType: "image/svg+xml", ContentDisposition: "attachment; filename=logo.svg"}) {
		t.Fatalf("logo metadata %+v", got)
	}
	if stored, err := store.Get(ctx, page.ID); err != nil || stored.Type != "application/octet-stream" {
		t.Fatalf("page record type %q err %v", stored.Type, err)
	}

	processed, done, err = media.BackfillServingPolicy(ctx, store, blobs)
	if err != nil || processed != 0 || !done {
		t.Fatalf("second processed %d done %v err %v", processed, done, err)
	}
}

func TestBackfillServingPolicySkipsUploadsMadeUnderThePolicy(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := media.NewMemoryStore()
	blobs := media.NewMemoryBlob()
	svc := media.NewService(store, blobs, authz.NewAuthorizer(authz.DefaultPolicy()), "")
	p := authz.Principal{ID: uuid.MustParse("45454545-4545-4545-4545-454545454545").String()}
	if _, err := svc.Upload(ctx, p, "notes.txt", "text/plain", []byte("notes")); err != nil {
		t.Fatal(err)
	}

	processed, done, err := media.BackfillServingPolicy(ctx, store, blobs)
	if err != nil || processed != 0 || !done {
		t.Fatalf("processed %d done %v err %v", processed, done, err)
	}
}

func TestBackfillServingPolicyLeavesInlineMediaInline(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := media.NewMemoryStore()
	blobs := media.NewMemoryBlob()
	photo := legacyMedia(t, store, blobs, media.Media{Name: "photo.png", Type: "image/png", Kind: media.KindImage, Key: "images/photo"})
	cv := legacyMedia(t, store, blobs, media.Media{Name: "cv.pdf", Type: "application/pdf", Kind: media.KindFile, Key: "files/cv"})

	if _, _, err := media.BackfillServingPolicy(ctx, store, blobs); err != nil {
		t.Fatal(err)
	}
	for _, item := range []media.Media{photo, cv} {
		if got, _ := blobs.Metadata(item.Key); got != (media.BlobMetadata{ContentType: item.Type}) {
			t.Errorf("%s metadata %+v", item.Name, got)
		}
		if stored, err := store.Get(ctx, item.ID); err != nil || stored.Type != item.Type {
			t.Errorf("%s record type %q err %v", item.Name, stored.Type, err)
		}
	}
}

func TestBackfillServingPolicyCoversArchivedMediaButNotPurgedBlobs(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := media.NewMemoryStore()
	blobs := media.NewMemoryBlob()
	archived := legacyMedia(t, store, blobs, media.Media{Name: "old.html", Type: "text/html", Kind: media.KindFile, Key: "files/old"})
	if err := store.Archive(ctx, archived.ID, nil); err != nil {
		t.Fatal(err)
	}
	purgedAt := time.Now().UTC()
	purged, err := store.Create(ctx, media.Media{
		Name: "gone.html", Type: "text/html", Kind: media.KindFile, Key: "files/gone", UploadedBy: uuid.New(),
		DeletedAt: &purgedAt, BlobPurgeStartedAt: &purgedAt, BlobPurgedAt: &purgedAt,
	})
	if err != nil {
		t.Fatal(err)
	}

	processed, _, err := media.BackfillServingPolicy(ctx, store, blobs)
	if err != nil || processed != 1 {
		t.Fatalf("processed %d err %v", processed, err)
	}
	if got, _ := blobs.Metadata(archived.Key); got.ContentType != "application/octet-stream" {
		t.Fatalf("archived metadata %+v", got)
	}
	if stored, _ := store.GetIncludingDeleted(ctx, purged.ID); stored.Type != "text/html" {
		t.Fatalf("purged record changed to %q", stored.Type)
	}
}

func TestBackfillServingPolicyDoesNotStallOnAnObjectThatIsGone(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := media.NewMemoryStore()
	blobs := media.NewMemoryBlob()
	if _, err := store.Create(ctx, media.Media{Name: "lost.html", Type: "text/html", Kind: media.KindFile, Key: "files/lost", UploadedBy: uuid.New()}); err != nil {
		t.Fatal(err)
	}
	page := legacyMedia(t, store, blobs, media.Media{Name: "page.html", Type: "text/html", Kind: media.KindFile, Key: "files/page"})

	if _, _, err := media.BackfillServingPolicy(ctx, store, blobs); err != nil {
		t.Fatal(err)
	}
	if got, _ := blobs.Metadata(page.Key); got.ContentType != "application/octet-stream" {
		t.Fatalf("page metadata %+v", got)
	}
	processed, done, err := media.BackfillServingPolicy(ctx, store, blobs)
	if err != nil || processed != 0 || !done {
		t.Fatalf("second processed %d done %v err %v", processed, done, err)
	}
}

func TestMaintainServingPolicyBackfillWorksThroughEveryBatch(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	store := media.NewMemoryStore()
	blobs := media.NewMemoryBlob()
	var keys []string
	for i := range 60 {
		item := legacyMedia(t, store, blobs, media.Media{Name: "page.html", Type: "text/html", Kind: media.KindFile, Key: "files/page-" + strconv.Itoa(i)})
		keys = append(keys, item.Key)
	}

	media.MaintainServingPolicyBackfill(ctx, store, blobs, time.Millisecond, func(err error) { t.Error(err) })

	deadline := time.Now().Add(5 * time.Second)
	for _, key := range keys {
		for {
			if got, _ := blobs.Metadata(key); got.ContentType == "application/octet-stream" {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s was never rewritten", key)
			}
			time.Sleep(time.Millisecond)
		}
	}
}
