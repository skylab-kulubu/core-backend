package media_test

import (
	"context"
	"errors"
	"reflect"
	"strconv"
	"strings"
	"sync"
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

	report, err := media.BackfillServingPolicy(ctx, store, blobs, nil)
	if err != nil || report != (media.BackfillReport{Applied: 2}) {
		t.Fatalf("report %+v err %v", report, err)
	}
	if got, _ := blobs.Metadata(page.Key); got != (media.BlobMetadata{ContentType: "application/octet-stream", ContentDisposition: "attachment; filename=page.html"}) {
		t.Fatalf("page metadata %+v", got)
	}
	if got, _ := blobs.Metadata(logo.Key); got != (media.BlobMetadata{ContentType: "image/svg+xml", ContentDisposition: "attachment; filename=logo.svg"}) {
		t.Fatalf("logo metadata %+v", got)
	}
	if stored, err := store.Get(ctx, page.ID); err != nil || stored.Type != "text/html" || !stored.UpdatedAt.Equal(page.UpdatedAt) {
		t.Fatalf("page record type %q updated %v (was %v) err %v", stored.Type, stored.UpdatedAt, page.UpdatedAt, err)
	}

	report, err = media.BackfillServingPolicy(ctx, store, blobs, nil)
	if err != nil || report != (media.BackfillReport{}) {
		t.Fatalf("second report %+v err %v", report, err)
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

	report, err := media.BackfillServingPolicy(ctx, store, blobs, nil)
	if err != nil || report != (media.BackfillReport{}) {
		t.Fatalf("report %+v err %v", report, err)
	}
}

func TestBackfillServingPolicyLeavesInlineMediaInline(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := media.NewMemoryStore()
	blobs := media.NewMemoryBlob()
	photo := legacyMedia(t, store, blobs, media.Media{Name: "photo.png", Type: "image/png", Kind: media.KindImage, Key: "images/photo"})
	cv := legacyMedia(t, store, blobs, media.Media{Name: "cv.pdf", Type: "application/pdf", Kind: media.KindFile, Key: "files/cv"})

	if _, err := media.BackfillServingPolicy(ctx, store, blobs, nil); err != nil {
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

	report, err := media.BackfillServingPolicy(ctx, store, blobs, nil)
	if err != nil || report != (media.BackfillReport{Applied: 1}) {
		t.Fatalf("report %+v err %v", report, err)
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

	if _, err := media.BackfillServingPolicy(ctx, store, blobs, nil); err != nil {
		t.Fatal(err)
	}
	if got, _ := blobs.Metadata(page.Key); got.ContentType != "application/octet-stream" {
		t.Fatalf("page metadata %+v", got)
	}
	report, err := media.BackfillServingPolicy(ctx, store, blobs, nil)
	if err != nil || report != (media.BackfillReport{}) {
		t.Fatalf("second report %+v err %v", report, err)
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

// failingBlob answers SetMetadata for one key with an error that is not
// ErrNotFound, as an unreachable or refusing R2 would, until healed.
type failingBlob struct {
	*media.MemoryBlob
	mu      sync.Mutex
	failKey string
}

func (b *failingBlob) SetMetadata(ctx context.Context, key string, meta media.BlobMetadata) error {
	b.mu.Lock()
	fail := key == b.failKey
	b.mu.Unlock()
	if fail {
		return errors.New("r2: internal error")
	}
	return b.MemoryBlob.SetMetadata(ctx, key, meta)
}

func (b *failingBlob) heal() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.failKey = ""
}

func TestBackfillServingPolicyGoesPastARecordThatFails(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := media.NewMemoryStore()
	blobs := media.NewMemoryBlob()
	first := legacyMedia(t, store, blobs, media.Media{ID: uuid.MustParse("00000000-0000-0000-0000-000000000001"), Name: "first.html", Type: "text/html", Kind: media.KindFile, Key: "files/first"})
	var later []media.Media
	for i := range 30 {
		later = append(later, legacyMedia(t, store, blobs, media.Media{Name: "later.html", Type: "text/html", Kind: media.KindFile, Key: "files/later-" + strconv.Itoa(i)}))
	}
	failing := &failingBlob{MemoryBlob: blobs, failKey: first.Key}
	var reported []error

	report, err := media.BackfillServingPolicy(ctx, store, failing, func(err error) { reported = append(reported, err) })
	if err != nil || report != (media.BackfillReport{Applied: 30, Failed: 1}) {
		t.Fatalf("report %+v err %v", report, err)
	}
	if len(reported) != 1 || !strings.Contains(reported[0].Error(), first.ID.String()) {
		t.Fatalf("reported %v", reported)
	}
	for _, item := range later {
		if got, _ := blobs.Metadata(item.Key); got.ContentType != "application/octet-stream" {
			t.Fatalf("%s left behind the failing record: %+v", item.Key, got)
		}
	}

	failing.heal()
	report, err = media.BackfillServingPolicy(ctx, store, failing, nil)
	if err != nil || report != (media.BackfillReport{Applied: 1}) {
		t.Fatalf("retry report %+v err %v", report, err)
	}
	if got, _ := blobs.Metadata(first.Key); got.ContentType != "application/octet-stream" {
		t.Fatalf("failed record was not retried: %+v", got)
	}
}

func TestMaintainServingPolicyBackfillRetriesAFailedRecordOnTheNextPass(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	store := media.NewMemoryStore()
	blobs := media.NewMemoryBlob()
	page := legacyMedia(t, store, blobs, media.Media{Name: "page.html", Type: "text/html", Kind: media.KindFile, Key: "files/page"})
	failing := &failingBlob{MemoryBlob: blobs, failKey: page.Key}
	reported := make(chan error, 1)

	media.MaintainServingPolicyBackfill(ctx, store, failing, time.Millisecond, func(err error) {
		select {
		case reported <- err:
		default:
		}
	})
	select {
	case <-reported:
	case <-time.After(5 * time.Second):
		t.Fatal("the failing record was never reported")
	}
	failing.heal()

	deadline := time.Now().Add(5 * time.Second)
	for {
		if got, _ := blobs.Metadata(page.Key); got.ContentType == "application/octet-stream" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the failed record was never retried")
		}
		time.Sleep(time.Millisecond)
	}
}
