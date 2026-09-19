package media_test

import (
	"context"
	"reflect"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/media"
)

func TestBackfillCoverColorsProcessesExistingImages(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := media.NewMemoryStore()
	blobs := media.NewMemoryBlob()
	key := "images/existing"
	if err := blobs.Put(ctx, key, twoTonePNG(t), "image/png"); err != nil {
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
