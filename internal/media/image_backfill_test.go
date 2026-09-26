package media_test

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"reflect"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/media"
)

// storedBefore stores an object and its Media record the way core did before
// image sizes: no size recorded, no sizes made.
func storedBefore(t *testing.T, store *media.MemoryStore, blobs media.BlobStore, key, ctype string, data []byte) media.Media {
	t.Helper()
	ctx := context.Background()
	if data != nil {
		if err := blobs.Put(ctx, key, data, media.BlobMetadata{ContentType: ctype}); err != nil {
			t.Fatal(err)
		}
	}
	created, err := store.Create(ctx, media.Media{Name: key, Type: ctype, Kind: media.KindImage, Key: key, UploadedBy: uuid.New()})
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

func TestBackfillImageSizesMakesTheSizesOfImagesStoredBefore(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := media.NewMemoryStore()
	memory := media.NewMemoryBlob()
	blobs := failingReads{MemoryBlob: memory, key: "images/unreachable"}
	catalogue, err := media.LoadCatalogue()
	if err != nil {
		t.Fatal(err)
	}

	// A legacy phone photo, stripped but not re-encoded: its EXIF says to
	// turn it a quarter to the right, so it shows 1200×1600.
	phone := withJPEGSegment(solidJPEG(t, 1600, 1200, color.RGBA{R: 30, G: 120, B: 60, A: 255}), 0xE1, orientationEXIF(6))
	photo := storedBefore(t, store, blobs, "images/photo", "image/jpeg", phone)
	small := storedBefore(t, store, blobs, "images/small", "image/png", solidPNG(t, 300, 200, color.RGBA{B: 90, A: 255}))
	broken := storedBefore(t, store, blobs, "images/broken", "image/png", []byte("\x89PNG\r\n\x1a\nbroken"))
	logo := storedBefore(t, store, blobs, "images/logo", "image/svg+xml", []byte(`<svg xmlns="http://www.w3.org/2000/svg"/>`))
	gone := storedBefore(t, store, blobs, "images/gone", "image/png", nil)
	unreachable := storedBefore(t, store, blobs, "images/unreachable", "image/png", solidPNG(t, 900, 900, color.RGBA{G: 90, A: 255}))

	var reported []error
	report, err := media.BackfillImageSizes(ctx, store, blobs, catalogue, func(err error) { reported = append(reported, err) })
	if err != nil {
		t.Fatal(err)
	}
	if report != (media.BackfillReport{Applied: 5, Failed: 1}) || len(reported) != 1 {
		t.Fatalf("report %+v, reported %v", report, reported)
	}

	got, _ := store.Get(ctx, photo.ID)
	wantSizes := map[string]media.ImageSize{"card": {Width: 300, Height: 400}, "page": {Width: 900, Height: 1200}}
	if got.Width != 1200 || got.Height != 1600 || !reflect.DeepEqual(got.SizeObjects, wantSizes) {
		t.Fatalf("photo recorded %d×%d %v", got.Width, got.Height, got.SizeObjects)
	}
	for size, want := range wantSizes {
		stored, ok := memory.Get("images/photo/" + size)
		if !ok {
			t.Fatalf("%s not stored", size)
		}
		img, format := decodeStored(t, stored)
		if format != "jpeg" || img.Bounds().Size() != image.Pt(want.Width, want.Height) {
			t.Errorf("%s stored %s %v", size, format, img.Bounds().Size())
		}
		if meta, _ := memory.Metadata("images/photo/" + size); meta != (media.BlobMetadata{ContentType: "image/jpeg"}) {
			t.Errorf("%s metadata %+v", size, meta)
		}
	}
	if original, _ := memory.Get("images/photo"); !bytes.Equal(original, phone) {
		t.Fatal("the backfill rewrote a legacy original")
	}

	for _, done := range []media.Media{small, broken, logo, gone} {
		got, _ := store.Get(ctx, done.ID)
		if got.SizeObjects == nil || len(got.SizeObjects) != 0 {
			t.Errorf("%s: recorded %v, want no sizes and nothing left to do", done.Key, got.SizeObjects)
		}
	}
	if got, _ := store.Get(ctx, small.ID); got.Width != 300 || got.Height != 200 {
		t.Errorf("small recorded %d×%d", got.Width, got.Height)
	}
	if got, _ := store.Get(ctx, unreachable.ID); got.SizeObjects != nil {
		t.Fatalf("an image whose read failed is recorded %v, want it left for the next pass", got.SizeObjects)
	}

	again, err := media.BackfillImageSizes(ctx, store, blobs, catalogue, nil)
	if err != nil || again != (media.BackfillReport{Failed: 1}) {
		t.Fatalf("second pass %+v %v", again, err)
	}
}
