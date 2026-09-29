package media_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
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
	// R2 wants every part but the last at least 5 MiB, all of one size.
	first := append([]byte("%PDF-1.7 "), bytes.Repeat([]byte("."), 5<<20-9)...)
	var parts []media.UploadedPart
	for i, chunk := range [][]byte{first, []byte("rest")} {
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
	// The public bucket serves every key at the CDN, pending ones too: the
	// joined object downloads there, it never renders.
	pending, ok := fake.Object("media", "pending/two")
	if !ok || pending.ContentType != "application/octet-stream" || pending.ContentDisposition != "attachment" {
		t.Fatalf("pending object %+v (found %v)", pending, ok)
	}
	size, err := r2.Size(ctx, "pending/two")
	if err != nil || size != 5<<20+4 {
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
	if !ok || !bytes.Equal(copied.Data, append(first, "rest"...)) || copied.ContentType != "application/octet-stream" ||
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

// The ZIP check reads a held file by ranges (ranged GETs): exactly the bytes
// asked for, ErrNotFound for a file that is gone, and an error, never other
// bytes, when storage answers a range it was not asked for. The bucket in
// memory reads the same.
func TestR2_OpenRangeReadsExactlyTheRangeAsked(t *testing.T) {
	t.Parallel()
	r2, _ := multipartR2(t)
	memory := media.NewMemoryBlob()
	ctx := context.Background()
	data := []byte("0123456789abcdefghij")
	for name, bucket := range map[string]media.ScanStorage{"r2": r2, "memory": memory} {
		t.Run(name, func(t *testing.T) {
			if err := bucket.(interface {
				Put(context.Context, string, []byte, media.BlobMetadata) error
			}).Put(ctx, "pending/scan/held", data, media.BlobMetadata{ContentType: "application/octet-stream"}); err != nil {
				t.Fatal(err)
			}
			for _, r := range []struct{ off, n int64 }{{0, 20}, {0, 1}, {5, 10}, {19, 1}} {
				body, err := bucket.OpenRange(ctx, "pending/scan/held", r.off, r.n)
				if err != nil {
					t.Fatalf("%+v: %v", r, err)
				}
				got, err := io.ReadAll(body)
				body.Close()
				if err != nil || !bytes.Equal(got, data[r.off:r.off+r.n]) {
					t.Fatalf("%+v: read %q, err %v", r, got, err)
				}
			}
			if size, err := bucket.Size(ctx, "pending/scan/held"); err != nil || size != 20 {
				t.Fatalf("size %d, err %v", size, err)
			}
			if _, err := bucket.OpenRange(ctx, "pending/scan/gone", 0, 1); !errors.Is(err, media.ErrNotFound) {
				t.Fatalf("a file that is gone: err = %v", err)
			}
			for _, r := range []struct{ off, n int64 }{{20, 1}, {15, 10}, {-1, 2}, {0, 0}} {
				if body, err := bucket.OpenRange(ctx, "pending/scan/held", r.off, r.n); err == nil {
					got, readErr := io.ReadAll(body)
					body.Close()
					if readErr == nil {
						t.Fatalf("%+v outside the file: read %q", r, got)
					}
				}
			}
		})
	}
}

// R2 joins parts only when every part but the last is the same size and at
// least 5 MiB; the fake refuses the others as R2 does.
func TestR2_PartsFollowR2sSizeRules(t *testing.T) {
	t.Parallel()
	r2, _ := multipartR2(t)
	ctx := context.Background()
	for name, sizes := range map[string][]int{
		"a small part before the last": {5 << 20, 4 << 20, 1},
		"parts of two sizes":           {6 << 20, 5 << 20, 1},
		"a last part larger":           {5 << 20, 6 << 20},
	} {
		key := "videos/" + strings.ReplaceAll(name, " ", "-") + ".fs.mp4"
		id, err := r2.CreateMultipartWith(ctx, key, media.BlobMetadata{ContentType: "video/mp4"})
		if err != nil {
			t.Fatal(err)
		}
		var parts []media.UploadedPart
		for i, size := range sizes {
			etag, err := r2.UploadPart(ctx, key, id, int32(i+1), make([]byte, size))
			if err != nil {
				t.Fatal(err)
			}
			parts = append(parts, media.UploadedPart{Number: int32(i + 1), ETag: etag})
		}
		if err := r2.CompleteMultipart(ctx, key, id, parts); !errors.Is(err, media.ErrMultipartPartsMismatch) {
			t.Errorf("%s: %v, want the parts refused", name, err)
		}
	}
}

// Core builds a video's faststart copy as a multipart upload of its own:
// the parts it rewrote from memory, and the rest copied by storage from the
// source object's byte ranges (UploadPartCopy), never downloaded. The object
// is stored with the metadata the upload was opened with.
func TestR2_BuildsAnObjectFromPartsAndCopiedRanges(t *testing.T) {
	t.Parallel()
	r2, fake := multipartR2(t)
	ctx := context.Background()
	source := make([]byte, 12<<20)
	for i := range source {
		source[i] = byte(i / 4096)
	}
	if err := r2.Put(ctx, "videos/a.mp4", source, media.BlobMetadata{ContentType: "video/mp4"}); err != nil {
		t.Fatal(err)
	}
	key := "videos/a.fs.mp4"
	id, err := r2.CreateMultipartWith(ctx, key, media.BlobMetadata{ContentType: "video/mp4"})
	if err != nil {
		t.Fatal(err)
	}
	head := bytes.Repeat([]byte("h"), 5<<20)
	first, err := r2.UploadPart(ctx, key, id, 1, head)
	if err != nil {
		t.Fatal(err)
	}
	second, err := r2.UploadPartCopy(ctx, key, id, 2, "videos/a.mp4", 1<<20, 5<<20)
	if err != nil {
		t.Fatal(err)
	}
	last, err := r2.UploadPartCopy(ctx, key, id, 3, "videos/a.mp4", 6<<20, 2<<20)
	if err != nil {
		t.Fatal(err)
	}
	if err := r2.CompleteMultipart(ctx, key, id, []media.UploadedPart{{Number: 1, ETag: first}, {Number: 2, ETag: second}, {Number: 3, ETag: last}}); err != nil {
		t.Fatal(err)
	}
	stored, ok := fake.Object("media", key)
	want := append(append(append([]byte{}, head...), source[1<<20:6<<20]...), source[6<<20:8<<20]...)
	if !ok || !bytes.Equal(stored.Data, want) || stored.ContentType != "video/mp4" || stored.ContentDisposition != "" {
		t.Fatalf("stored %d bytes as %q %q (found %v)", len(stored.Data), stored.ContentType, stored.ContentDisposition, ok)
	}
	if n := fake.Count("UploadPartCopy"); n != 2 {
		t.Fatalf("%d part copies, want 2", n)
	}
	if _, err := r2.UploadPartCopy(ctx, key, "no-such-upload", 1, "videos/a.mp4", 0, 1); !errors.Is(err, media.ErrMultipartGone) {
		t.Fatalf("a part copy into an upload that is gone: %v", err)
	}
	id, err = r2.CreateMultipartWith(ctx, key, media.BlobMetadata{ContentType: "video/mp4"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r2.UploadPartCopy(ctx, key, id, 1, "videos/gone.mp4", 0, 1); !errors.Is(err, media.ErrNotFound) {
		t.Fatalf("a part copy of an object that is gone: %v", err)
	}
	// Deleting a faststart copy's key aborts the multipart upload open at
	// it too: a rewrite cut short leaves no parts behind.
	if err := r2.Delete(ctx, key); err != nil {
		t.Fatal(err)
	}
	if _, ok := fake.Object("media", key); ok || len(fake.OpenUploads("media")) != 0 || !slices.Equal(fake.Aborted(), []string{key}) {
		t.Fatalf("after the delete: open %v, aborted %v", fake.OpenUploads("media"), fake.Aborted())
	}
}
