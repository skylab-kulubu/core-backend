package media_test

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"testing"
	"time"

	"github.com/skylab-kulubu/core-backend/internal/media"
	"github.com/skylab-kulubu/core-backend/internal/media/s3test"
)

func multipartR2(t *testing.T) (*media.R2, *s3test.Server) {
	t.Helper()
	fake := s3test.New(t)
	return media.NewR2(media.R2Config{Endpoint: fake.URL, AccessKey: "access", SecretKey: "secret", Bucket: "media"}), fake
}

// putPart sends a part to its presigned address the way a browser does:
// a plain PUT of the bytes, nothing but the address to go on.
func putPart(t *testing.T, address string, data []byte) string {
	t.Helper()
	req, err := http.NewRequest(http.MethodPut, address, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("part PUT: status %d", resp.StatusCode)
	}
	return resp.Header.Get("ETag")
}

// A part's address is signed for its exact length, so a browser cannot send
// more than the part it was given, and it expires.
func TestR2_PresignedPartAddressSignsItsLengthAndExpires(t *testing.T) {
	t.Parallel()
	r2, fake := multipartR2(t)
	ctx := context.Background()
	id, err := r2.CreateMultipart(ctx, "pending/one")
	if err != nil {
		t.Fatal(err)
	}

	address, err := r2.PresignPart(ctx, "pending/one", id, 1, 5, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(address)
	if err != nil {
		t.Fatal(err)
	}
	q := parsed.Query()
	if parsed.Path != "/media/pending/one" || q.Get("partNumber") != "1" || q.Get("uploadId") != id ||
		q.Get("X-Amz-Expires") != "3600" || q.Get("X-Amz-SignedHeaders") != "content-length;host" {
		t.Fatalf("presigned address %s", address)
	}
	etag := putPart(t, address, []byte("hello"))

	parts, err := r2.ListParts(ctx, "pending/one", id)
	if err != nil {
		t.Fatal(err)
	}
	if len(parts) != 1 || parts[0].Number != 1 || parts[0].Size != 5 || parts[0].ETag != etag {
		t.Fatalf("parts %+v, want part 1 of 5 bytes with ETag %s", parts, etag)
	}
	if len(fake.PartQueries()) != 1 {
		t.Fatalf("part uploads %d", len(fake.PartQueries()))
	}
}

func TestR2_CompletesAMultipartUploadThenReadsCopiesAndSizesIt(t *testing.T) {
	t.Parallel()
	r2, fake := multipartR2(t)
	ctx := context.Background()
	id, err := r2.CreateMultipart(ctx, "pending/two")
	if err != nil {
		t.Fatal(err)
	}
	var parts []media.UploadedPart
	for i, chunk := range [][]byte{[]byte("%PDF-1.7 "), []byte("rest")} {
		number := int32(i + 1)
		address, err := r2.PresignPart(ctx, "pending/two", id, number, int64(len(chunk)), time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		parts = append(parts, media.UploadedPart{Number: number, ETag: putPart(t, address, chunk)})
	}

	if err := r2.CompleteMultipart(ctx, "pending/two", id, parts); err != nil {
		t.Fatal(err)
	}
	if open := fake.OpenUploads("media"); len(open) != 0 {
		t.Fatalf("open uploads after completion %v", open)
	}
	size, err := r2.Size(ctx, "pending/two")
	if err != nil || size != 13 {
		t.Fatalf("size %d err %v", size, err)
	}
	start, err := r2.ReadStart(ctx, "pending/two", 5)
	if err != nil || string(start) != "%PDF-" {
		t.Fatalf("start %q err %v", start, err)
	}
	if err := r2.Copy(ctx, "pending/two", "files/two", media.BlobMetadata{
		ContentType: "application/octet-stream", ContentDisposition: "attachment; filename=two.zip",
	}); err != nil {
		t.Fatal(err)
	}
	copied, ok := fake.Object("media", "files/two")
	if !ok || string(copied.Data) != "%PDF-1.7 rest" || copied.ContentType != "application/octet-stream" ||
		copied.ContentDisposition != "attachment; filename=two.zip" {
		t.Fatalf("copy %+v", copied)
	}

	if _, err := r2.Size(ctx, "pending/missing"); !errors.Is(err, media.ErrNotFound) {
		t.Fatalf("size of a missing object: %v", err)
	}
	if _, err := r2.ReadStart(ctx, "pending/missing", 5); !errors.Is(err, media.ErrNotFound) {
		t.Fatalf("start of a missing object: %v", err)
	}
}

// A small file's start is the whole file: reading more than there is is
// not an error.
func TestR2_ReadStartOfAFileShorterThanAsked(t *testing.T) {
	t.Parallel()
	r2, _ := multipartR2(t)
	ctx := context.Background()
	if err := r2.Put(ctx, "pending/short", []byte("PK"), media.BlobMetadata{ContentType: "application/octet-stream"}); err != nil {
		t.Fatal(err)
	}
	start, err := r2.ReadStart(ctx, "pending/short", 512)
	if err != nil || string(start) != "PK" {
		t.Fatalf("start %q err %v", start, err)
	}
}

func TestR2_MultipartRefusalsAreTold(t *testing.T) {
	t.Parallel()
	r2, _ := multipartR2(t)
	ctx := context.Background()
	id, err := r2.CreateMultipart(ctx, "pending/three")
	if err != nil {
		t.Fatal(err)
	}
	address, err := r2.PresignPart(ctx, "pending/three", id, 1, 4, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	putPart(t, address, []byte("data"))

	err = r2.CompleteMultipart(ctx, "pending/three", id, []media.UploadedPart{{Number: 1, ETag: `"not-its-etag"`}})
	if !errors.Is(err, media.ErrMultipartPartsMismatch) {
		t.Fatalf("completing with a wrong ETag: %v", err)
	}
	if _, err := r2.ListParts(ctx, "pending/three", "no-such-upload"); !errors.Is(err, media.ErrMultipartGone) {
		t.Fatalf("parts of an upload that is gone: %v", err)
	}
	if err := r2.CompleteMultipart(ctx, "pending/three", "no-such-upload", []media.UploadedPart{{Number: 1, ETag: `"x"`}}); !errors.Is(err, media.ErrMultipartGone) {
		t.Fatalf("completing an upload that is gone: %v", err)
	}
}

// Every path that deletes by key alone (the staging sweeper, account
// erasure, a refused completion) deletes a pending key; that also aborts the
// multipart uploads still open at exactly that key, so no parts are left.
func TestR2_DeletingAPendingKeyAbortsItsOpenUploads(t *testing.T) {
	t.Parallel()
	r2, fake := multipartR2(t)
	ctx := context.Background()
	for _, key := range []string{"pending/a", "pending/a", "pending/ab", "files/a"} {
		if _, err := r2.CreateMultipart(ctx, key); err != nil {
			t.Fatal(err)
		}
	}

	if err := r2.Delete(ctx, "pending/a"); err != nil {
		t.Fatal(err)
	}
	if aborted := fake.Aborted(); !slices.Equal(aborted, []string{"pending/a", "pending/a"}) {
		t.Fatalf("aborted %v", aborted)
	}
	if open := fake.OpenUploads("media"); !slices.Equal(open, []string{"files/a", "pending/ab"}) {
		t.Fatalf("open %v", open)
	}
	// Deleting it again is still a success.
	if err := r2.Delete(ctx, "pending/a"); err != nil {
		t.Fatal(err)
	}
	// Only pending keys are Direct uploads' multipart uploads.
	if err := r2.Delete(ctx, "files/a"); err != nil {
		t.Fatal(err)
	}
	if open := fake.OpenUploads("media"); !slices.Equal(open, []string{"files/a", "pending/ab"}) {
		t.Fatalf("open after deleting files/a %v", open)
	}
}
