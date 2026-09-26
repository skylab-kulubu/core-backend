package media_test

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/media"
)

// storedBefore stores an object and its Media record the way core did
// before image sizes: no size recorded, no sizes made.
func storedBefore(t *testing.T, store *media.MemoryStore, blobs media.BlobStore, purpose, key, ctype string, data []byte) media.Media {
	t.Helper()
	ctx := context.Background()
	if data != nil {
		if err := blobs.Put(ctx, key, data, media.BlobMetadata{ContentType: ctype}); err != nil {
			t.Fatal(err)
		}
	}
	created, err := store.Create(ctx, media.Media{Name: key, Type: ctype, Kind: media.KindImage, Key: key, UploadedBy: uuid.New(), Purpose: purpose})
	if err != nil {
		t.Fatal(err)
	}
	return created
}

// failingReads is a blob store whose reads of one key fail, as a network
// error would.
type failingReads struct {
	*media.MemoryBlob
	key string
}

func (b failingReads) Read(ctx context.Context, key string) ([]byte, error) {
	if key == b.key {
		return nil, errors.New("connection reset")
	}
	return b.MemoryBlob.Read(ctx, key)
}

func reviewedCatalogue(t *testing.T) media.Catalogue {
	t.Helper()
	catalogue, err := media.LoadCatalogue()
	if err != nil {
		t.Fatal(err)
	}
	return catalogue
}

func freeBudget() *media.DecodeBudget {
	return media.NewDecodeBudget(media.DecodeBudgetConfig{})
}

func TestBackfillImageSizesMakesTheSizesOfPurposedImagesStoredBefore(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := media.NewMemoryStore()
	memory := media.NewMemoryBlob()
	blobs := failingReads{MemoryBlob: memory, key: "images/unreachable"}

	// An Event cover stored before re-encoding: its EXIF says to turn it a
	// quarter to the right, so it shows 1200×1600.
	profile := iccProfile(4_000)
	phone := withJPEGICC(withJPEGSegment(solidJPEG(t, 1600, 1200, color.RGBA{R: 30, G: 120, B: 60, A: 255}), 0xE1, orientationEXIF(6)), profile)
	photo := storedBefore(t, store, blobs, "event_cover", "images/photo", "image/jpeg", phone)
	small := storedBefore(t, store, blobs, "event_cover", "images/small", "image/png", solidPNG(t, 300, 200, color.RGBA{B: 90, A: 255}))
	broken := storedBefore(t, store, blobs, "event_cover", "images/broken", "image/png", []byte("\x89PNG\r\n\x1a\nbroken"))
	gone := storedBefore(t, store, blobs, "event_cover", "images/gone", "image/png", nil)
	elsewhere := storedBefore(t, store, blobs, "event_cover", "uploads/elsewhere.png", "image/png", solidPNG(t, 900, 900, color.RGBA{G: 9, A: 255}))
	unreachable := storedBefore(t, store, blobs, "event_cover", "images/unreachable", "image/png", solidPNG(t, 900, 900, color.RGBA{G: 90, A: 255}))
	// Uploaded without a purpose: no sizes until ticket 08 gives it one.
	legacy := storedBefore(t, store, blobs, media.PurposeLegacy, "images/legacy", "image/jpeg", solidJPEG(t, 1600, 1200, color.RGBA{R: 200, A: 255}))

	var reported []error
	report, err := media.BackfillImageSizes(ctx, store, blobs, reviewedCatalogue(t), freeBudget(), func(err error) { reported = append(reported, err) })
	if err != nil {
		t.Fatal(err)
	}
	if report != (media.BackfillReport{Applied: 5, Failed: 1}) || len(reported) != 1 {
		t.Fatalf("report %+v, reported %v", report, reported)
	}

	got, _ := store.Get(ctx, photo.ID)
	want := map[string]media.SizeObject{
		"card": {ImageSize: media.ImageSize{Width: 300, Height: 400}, Type: "image/jpeg"},
		"page": {ImageSize: media.ImageSize{Width: 900, Height: 1200}, Type: "image/jpeg"},
	}
	if got.Width != 1200 || got.Height != 1600 || !reflect.DeepEqual(got.SizeObjects, want) {
		t.Fatalf("photo recorded %d×%d %v", got.Width, got.Height, got.SizeObjects)
	}
	for size, object := range want {
		stored, ok := memory.Get("images/photo/" + size + ".jpg")
		if !ok {
			t.Fatalf("%s not stored", size)
		}
		if img, format := decodeStored(t, stored); format != "jpeg" || img.Bounds().Size() != image.Pt(object.Width, object.Height) {
			t.Errorf("%s stored %s %v", size, format, img.Bounds().Size())
		}
		if !bytes.Equal(jpegICC(t, stored), profile) {
			t.Errorf("%s lost the colour profile", size)
		}
	}
	if original, _ := memory.Get("images/photo"); !bytes.Equal(original, phone) {
		t.Fatal("the backfill rewrote an original")
	}
	for _, done := range []media.Media{small, broken, gone, elsewhere} {
		got, _ := store.Get(ctx, done.ID)
		if got.SizeObjects == nil || len(got.SizeObjects) != 0 {
			t.Errorf("%s: recorded %v, want no sizes and nothing left to do", done.Key, got.SizeObjects)
		}
	}
	for _, key := range memory.Keys() {
		if strings.HasPrefix(key, "uploads/elsewhere.png/") || strings.HasPrefix(key, "images/legacy/") {
			t.Errorf("stored %s", key)
		}
	}
	if got, _ := store.Get(ctx, small.ID); got.Width != 300 || got.Height != 200 {
		t.Errorf("small recorded %d×%d", got.Width, got.Height)
	}
	if got, _ := store.Get(ctx, unreachable.ID); got.SizeObjects != nil {
		t.Fatalf("an image whose read failed is recorded %v, want it left for the next pass", got.SizeObjects)
	}
	if got, _ := store.Get(ctx, legacy.ID); got.SizeObjects != nil {
		t.Fatalf("a legacy image is recorded %v", got.SizeObjects)
	}

	again, err := media.BackfillImageSizes(ctx, store, blobs, reviewedCatalogue(t), freeBudget(), nil)
	if err != nil || again != (media.BackfillReport{Failed: 1}) {
		t.Fatalf("second pass %+v %v", again, err)
	}
}

func TestBackfillImageSizesDoesNotDecodeAWebPFrameLargerThanItsCanvas(t *testing.T) {
	ctx := context.Background()
	store := media.NewMemoryStore()
	blobs := media.NewMemoryBlob()
	lie := storedBefore(t, store, blobs, media.PurposeProfilePicture, "images/lie", "image/webp", canvasLieWebP(t))

	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	report, err := media.BackfillImageSizes(ctx, store, blobs, reviewedCatalogue(t), freeBudget(), nil)
	runtime.ReadMemStats(&after)
	if err != nil || report != (media.BackfillReport{Applied: 1}) {
		t.Fatalf("report %+v %v", report, err)
	}
	if grew := after.TotalAlloc - before.TotalAlloc; grew > 32<<20 {
		t.Fatalf("the backfill allocated %d MiB for a %d-byte file", grew>>20, len(canvasLieWebP(t)))
	}
	if got, _ := store.Get(ctx, lie.ID); got.SizeObjects == nil || len(got.SizeObjects) != 0 {
		t.Fatalf("recorded %v, want done without sizes", got.SizeObjects)
	}
}

// panickingPuts is a blob store whose writes under one prefix panic, as a
// fault deep in an encoder or a client would.
type panickingPuts struct {
	*media.MemoryBlob
	prefix string
}

func (b panickingPuts) Put(ctx context.Context, key string, data []byte, meta media.BlobMetadata) error {
	if strings.HasPrefix(key, b.prefix) {
		panic("boom")
	}
	return b.MemoryBlob.Put(ctx, key, data, meta)
}

func TestBackfillImageSizesWalksPastAPanicAndDoesNotTryTheImageAgain(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := media.NewMemoryStore()
	memory := media.NewMemoryBlob()
	blobs := panickingPuts{MemoryBlob: memory, prefix: "images/boom/"}
	budget := media.NewDecodeBudget(media.DecodeBudgetConfig{Slots: 1, Wait: 50 * time.Millisecond})
	boom := storedBefore(t, store, memory, "event_cover", "images/boom", "image/png", solidPNG(t, 900, 600, color.RGBA{R: 1, A: 255}))
	fine := storedBefore(t, store, memory, "event_cover", "images/fine", "image/png", solidPNG(t, 900, 600, color.RGBA{G: 1, A: 255}))

	var reported []error
	report, err := media.BackfillImageSizes(ctx, store, blobs, reviewedCatalogue(t), budget, func(err error) { reported = append(reported, err) })
	if err != nil || report != (media.BackfillReport{Applied: 1, Failed: 1}) || len(reported) != 1 {
		t.Fatalf("report %+v %v, reported %v", report, err, reported)
	}
	if got, _ := store.Get(ctx, boom.ID); got.SizeObjects == nil || len(got.SizeObjects) != 0 {
		t.Fatalf("the image that panicked is recorded %v, want done without sizes", got.SizeObjects)
	}
	if got, _ := store.Get(ctx, fine.ID); len(got.SizeObjects) != 1 {
		t.Fatalf("the next image is recorded %v", got.SizeObjects)
	}
	release, err := budget.Acquire(ctx)
	if err != nil {
		t.Fatalf("the decoding slot was not given back: %v", err)
	}
	release()
	if again, err := media.BackfillImageSizes(ctx, store, blobs, reviewedCatalogue(t), budget, nil); err != nil || again != (media.BackfillReport{}) {
		t.Fatalf("a restart tries the image again: %+v %v", again, err)
	}
}

func TestBackfillImageSizesWaitsForADecodingSlotAndRetriesLater(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := media.NewMemoryStore()
	blobs := media.NewMemoryBlob()
	budget := media.NewDecodeBudget(media.DecodeBudgetConfig{Slots: 1, Wait: 20 * time.Millisecond})
	item := storedBefore(t, store, blobs, "event_cover", "images/waiting", "image/png", solidPNG(t, 900, 600, color.RGBA{B: 1, A: 255}))

	release, err := budget.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var reported []error
	report, err := media.BackfillImageSizes(ctx, store, blobs, reviewedCatalogue(t), budget, func(err error) { reported = append(reported, err) })
	release()
	if err != nil || report != (media.BackfillReport{Failed: 1}) || len(reported) != 1 || !errors.Is(reported[0], media.ErrDecodeBusy) {
		t.Fatalf("report %+v %v, reported %v", report, err, reported)
	}
	if got, _ := store.Get(ctx, item.ID); got.SizeObjects != nil {
		t.Fatalf("recorded %v while it waited", got.SizeObjects)
	}
	if report, err := media.BackfillImageSizes(ctx, store, blobs, reviewedCatalogue(t), budget, nil); err != nil || report != (media.BackfillReport{Applied: 1}) {
		t.Fatalf("the next pass: %+v %v", report, err)
	}
}
