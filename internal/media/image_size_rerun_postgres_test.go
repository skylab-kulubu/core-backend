package media_test

import (
	"context"
	"image/color"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/media"
)

// listedImageSizes is the Postgres store telling each time the image size
// backfill lists the images waiting for their sizes, and how many it found.
type listedImageSizes struct {
	*media.PostgresStore
	listed chan int
}

func (s listedImageSizes) ListPendingImageSizes(ctx context.Context, purposes []string, after uuid.UUID, limit int) ([]media.Media, error) {
	items, err := s.PostgresStore.ListPendingImageSizes(ctx, purposes, after, limit)
	select {
	case s.listed <- len(items):
	default:
	}
	return items, err
}

// backgroundErrors keeps what the background backfills report: they may
// still report after the test ends, when t can no longer be used.
type backgroundErrors struct {
	mu   sync.Mutex
	errs []error
}

func (b *backgroundErrors) add(err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.errs = append(b.errs, err)
}

func (b *backgroundErrors) list() []error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]error(nil), b.errs...)
}

// Core starts the image size backfill and the legacy purpose backfill side
// by side. The size backfill's first pass can end before the purpose
// backfill gives a legacy Event cover its purpose; the cover still gets its
// sizes, without a restart. A private image gets none.
func TestPostgresALegacyCoverGetsItsSizesWhenItsPurposeComesAfterTheSizeBackfill(t *testing.T) {
	db := newPrivateDatabase(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cover, err := db.svc.Upload(ctx, db.organizer, "cover.png", "image/png", solidPNG(t, 1600, 1000, color.RGBA{R: 200, A: 255}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.events().Create(ctx, db.organizer, event.Event{Name: "Hack", Location: "YTÜ", OwnerTeam: "WEBLAB", CoverImageID: &cover.ID}); err != nil {
		t.Fatal(err)
	}
	if got := db.get(t, cover.ID); got.Purpose != media.PurposeLegacy || got.SizeObjects != nil {
		t.Fatalf("cover stored as %q with size objects %v, want legacy with none", got.Purpose, got.SizeObjects)
	}
	private, err := db.svc.UploadForPurpose(ctx, db.organizer, "certificate_asset", uploaded("background.png", "image/png", largePNG(t)))
	if err != nil {
		t.Fatal(err)
	}
	catalogue := reviewedCatalogue(t)
	var reported backgroundErrors

	sizes := listedImageSizes{PostgresStore: db.store, listed: make(chan int, 64)}
	rerun := media.MaintainImageSizeBackfill(ctx, sizes, db.blobs, catalogue, freeBudget(), time.Millisecond, reported.add)
	select {
	case found := <-sizes.listed:
		if found != 0 {
			t.Fatalf("the first size pass found %d images, want none: the cover is still legacy", found)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the size backfill never listed the images waiting for their sizes")
	}
	media.MaintainLegacyPurposeBackfill(ctx, db.store, catalogue, time.Millisecond, rerun, nil, reported.add)

	want := map[string]media.SizeObject{
		"card": {ImageSize: media.ImageSize{Width: 400, Height: 250}, Type: "image/png"},
		"page": {ImageSize: media.ImageSize{Width: 1200, Height: 750}, Type: "image/png"},
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		// The backfill records the image as done without sizes before it
		// decodes it, and its sizes once they are stored.
		got := db.get(t, cover.ID)
		if len(got.SizeObjects) != 0 {
			if got.Purpose != media.PurposeEventCover || got.Width != 1600 || got.Height != 1000 || !reflect.DeepEqual(got.SizeObjects, want) {
				t.Fatalf("cover %q %d×%d with size objects %v", got.Purpose, got.Width, got.Height, got.SizeObjects)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("cover %q still has no sizes (background errors %v)", got.Purpose, reported.list())
		}
		time.Sleep(10 * time.Millisecond)
	}
	for _, size := range []string{"card.png", "page.png"} {
		if _, ok := db.blobs.Get(cover.Key + "/" + size); !ok {
			t.Fatalf("cover %s not stored", size)
		}
	}

	got := db.get(t, private.ID)
	if got.SizeObjects == nil || len(got.SizeObjects) != 0 {
		t.Fatalf("private image with size objects %v, want none made", got.SizeObjects)
	}
	for _, key := range db.blobs.Keys() {
		if strings.HasPrefix(key, private.Key) {
			t.Fatalf("public object %s beside the private image", key)
		}
	}
	if keys := db.private.Keys(); len(keys) != 1 {
		t.Fatalf("private bucket holds %v, want the image alone", keys)
	}
	if errs := reported.list(); len(errs) != 0 {
		t.Fatalf("background errors %v", errs)
	}
}
