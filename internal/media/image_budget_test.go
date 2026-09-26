package media_test

import (
	"context"
	"errors"
	"image"
	"image/color"
	"runtime"
	"testing"
	"time"

	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/media"
)

// allocated is how many bytes run allocates (TotalAlloc), after a GC.
func allocated(run func()) uint64 {
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	run()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

func TestService_PurposeRefusesAWebPWhoseFrameIsNotItsCanvas(t *testing.T) {
	svc, blobs := setup(t)
	p := signedIn("80808080-8080-8080-8080-808080808080")
	lie := canvasLieWebP(t)

	var err error
	grew := allocated(func() {
		_, err = svc.UploadForPurpose(context.Background(), p, "profile_picture", uploaded("lie.webp", "image/webp", lie))
	})
	if !errors.Is(err, media.ErrTypeNotAllowed) {
		t.Fatalf("err = %v, want %v", err, media.ErrTypeNotAllowed)
	}
	if grew > 32<<20 {
		t.Fatalf("the upload allocated %d MiB for a %d-byte file", grew>>20, len(lie))
	}
	if keys := blobs.Keys(); len(keys) != 0 {
		t.Fatalf("stored %v", keys)
	}
	// Without a purpose the WebP is stored as uploaded, stripped, and not
	// decoded for its sizes or cover colours either.
	grew = allocated(func() {
		_, err = svc.Upload(context.Background(), p, "lie.webp", "image/webp", lie)
	})
	if err != nil || grew > 32<<20 {
		t.Fatalf("legacy upload: err %v, allocated %d MiB", err, grew>>20)
	}
}

// jpegHeader is a JPEG's start, JFIF segment and frame header alone,
// claiming w×h pixels in three unsubsampled components: marker 0xC0
// (baseline) or 0xC2 (progressive).
func jpegHeader(marker byte, w, h int) []byte {
	out := []byte{0xFF, 0xD8}
	out = append(out, 0xFF, 0xE0, 0x00, 0x10, 'J', 'F', 'I', 'F', 0x00, 0x01, 0x01, 0x00, 0x00, 0x01, 0x00, 0x01, 0x00, 0x00)
	out = append(out, 0xFF, marker, 0x00, 0x11, 0x08, byte(h>>8), byte(h), byte(w>>8), byte(w), 0x03)
	for id := byte(1); id <= 3; id++ {
		out = append(out, id, 0x11, 0x00)
	}
	return append(out, 0xFF, 0xD9)
}

// A progressive JPEG keeps a 256-byte block of coefficients per 64 samples
// between scans: at 7000×7000 in three components, 735 MB where a baseline
// JPEG of the same size takes 147 MB.
func TestService_PurposeCountsAProgressiveJPEGsCoefficientsBeforeDecoding(t *testing.T) {
	t.Parallel()
	svc, _ := setup(t)
	p := signedIn("81818181-8181-8181-8181-818181818181")

	_, err := svc.UploadForPurpose(context.Background(), p, "profile_picture", uploaded("progressive.jpg", "image/jpeg", jpegHeader(0xC2, 7000, 7000)))
	var refusal *media.PurposeRefusal
	if !errors.Is(err, media.ErrImageTooLarge) || !errors.As(err, &refusal) {
		t.Fatalf("progressive: err = %v, want %v", err, media.ErrImageTooLarge)
	}
	if refusal.MaxPixels >= media.MaxImagePixels || refusal.MaxPixels < 10_000_000 {
		t.Fatalf("progressive: maxPixels %d, want the lower limit for its pixel cost", refusal.MaxPixels)
	}
	// The baseline header passes the estimate and fails only in the
	// decoder (it has no tables).
	_, err = svc.UploadForPurpose(context.Background(), p, "profile_picture", uploaded("baseline.jpg", "image/jpeg", jpegHeader(0xC0, 7000, 7000)))
	if !errors.Is(err, media.ErrTypeNotAllowed) {
		t.Fatalf("baseline: err = %v, want %v", err, media.ErrTypeNotAllowed)
	}
}

// A 24 MP phone photo turned by its EXIF: scaled down before it is turned,
// with a scaler whose buffers do not grow with the source. A kernel scaler
// alone would allocate 327 MB (source height × target width × 32 bytes),
// and turning the photo at full size another 192 MB.
func TestService_PurposeScalesALargePhotoWithinBoundedMemory(t *testing.T) {
	svc, blobs := setup(t)
	photo := withJPEGSegment(solidJPEGOf(t, 6000, 4000), 0xE1, orientationEXIF(6))

	var created media.Media
	var err error
	grew := allocated(func() {
		created, err = svc.UploadForPurpose(context.Background(), signedIn("82828282-8282-8282-8282-828282828282"), "profile_picture", uploaded("big.jpg", "image/jpeg", photo))
	})
	if err != nil {
		t.Fatal(err)
	}
	if grew > 240<<20 {
		t.Fatalf("the upload allocated %d MiB", grew>>20)
	}
	stored, _ := blobs.Get(created.Key)
	if img, _ := decodeStored(t, stored); img.Bounds().Size() != image.Pt(1707, 2560) {
		t.Fatalf("stored %v, want the photo upright at 1707×2560", img.Bounds().Size())
	}
}

// solidJPEGOf is a large one-colour JPEG, encoded without a pixel buffer.
func solidJPEGOf(t *testing.T, w, h int) []byte {
	t.Helper()
	return encodeJPEG(t, solid{w: w, h: h, c: color.RGBA{R: 120, G: 60, B: 30, A: 255}})
}

func TestService_UploadWaitsForADecodingSlotThenAnswersBusy(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, blobs := media.NewMemoryStore(), media.NewMemoryBlob()
	budget := media.NewDecodeBudget(media.DecodeBudgetConfig{Slots: 1, Wait: 20 * time.Millisecond})
	svc := media.NewServiceWithOptions(store, blobs, authz.NewAuthorizer(authz.DefaultPolicy()), "https://cdn.example.test",
		media.ServiceOptions{DecodeBudget: budget})
	p := signedIn("83838383-8383-8383-8383-838383838383")

	release, err := budget.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_, err = svc.UploadForPurpose(ctx, p, "profile_picture", uploaded("dot.png", "image/png", pngDot()))
	if !errors.Is(err, media.ErrDecodeBusy) || time.Since(start) > time.Second {
		t.Fatalf("err = %v after %s, want %v", err, time.Since(start), media.ErrDecodeBusy)
	}
	if keys := blobs.Keys(); len(keys) != 0 {
		t.Fatalf("stored %v", keys)
	}
	// Without a purpose nothing is decoded but the cover colours, which
	// wait for the backfill instead.
	legacy, err := svc.Upload(ctx, p, "dot.png", "image/png", pngDot())
	if err != nil {
		t.Fatalf("legacy upload while busy: %v", err)
	}
	if got, _ := store.Get(ctx, legacy.ID); got.CoverColorsComputed {
		t.Fatal("cover colours computed without a decoding slot")
	}
	release()
	if _, err := svc.UploadForPurpose(ctx, p, "profile_picture", uploaded("dot.png", "image/png", pngDot())); err != nil {
		t.Fatalf("after the slot freed: %v", err)
	}
}

func TestDecodeBudgetSanitizesOneSVGAtATime(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	budget := media.NewDecodeBudget(media.DecodeBudgetConfig{Slots: 3, Wait: 20 * time.Millisecond})

	release, err := budget.AcquireSVG(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := budget.AcquireSVG(ctx); !errors.Is(err, media.ErrDecodeBusy) {
		t.Fatalf("a second SVG: %v, want %v", err, media.ErrDecodeBusy)
	}
	other, err := budget.Acquire(ctx)
	if err != nil {
		t.Fatalf("a raster image beside the SVG: %v", err)
	}
	other()
	release()
	again, err := budget.AcquireSVG(ctx)
	if err != nil {
		t.Fatalf("after the SVG: %v", err)
	}
	again()

	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	held, _ := budget.AcquireSVG(ctx)
	defer held()
	if _, err := budget.AcquireSVG(cancelled); !errors.Is(err, context.Canceled) {
		t.Fatalf("a cancelled request: %v", err)
	}
}
