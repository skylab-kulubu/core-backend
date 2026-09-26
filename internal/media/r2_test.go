package media_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/skylab-kulubu/core-backend/internal/media"
)

type s3Request struct {
	method string
	path   string
	header http.Header
}

// fakeS3 answers the S3 calls the R2 adapter makes and records them, so the
// tests read the object metadata R2 would have been asked to store.
type fakeS3 struct {
	mu       sync.Mutex
	requests []s3Request
	// failures maps a request path to the S3 error code it answers with.
	failures map[string]string
}

func (f *fakeS3) fail(path, code string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failures == nil {
		f.failures = make(map[string]string)
	}
	f.failures[path] = code
}

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.requests = append(f.requests, s3Request{method: r.Method, path: r.URL.Path, header: r.Header.Clone()})
	code := f.failures[r.URL.Path]
	f.mu.Unlock()
	if code != "" {
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`<Error><Code>` + code + `</Code><Message>not found</Message></Error>`))
		return
	}
	w.Header().Set("ETag", `"etag"`)
	w.WriteHeader(http.StatusOK)
	if r.Header.Get("X-Amz-Copy-Source") != "" {
		_, _ = w.Write([]byte(`<CopyObjectResult><ETag>"etag"</ETag><LastModified>2026-09-25T00:00:00.000Z</LastModified></CopyObjectResult>`))
	}
}

func (f *fakeS3) writes() []s3Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []s3Request
	for _, r := range f.requests {
		if r.method == http.MethodPut {
			out = append(out, r)
		}
	}
	return out
}

func fakeR2(t *testing.T) (*media.R2, *fakeS3) {
	t.Helper()
	fake := &fakeS3{}
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	return media.NewR2(media.R2Config{Endpoint: srv.URL, AccessKey: "access", SecretKey: "secret", Bucket: "media"}), fake
}

func TestR2_PutStoresTheContentDisposition(t *testing.T) {
	t.Parallel()
	r2, fake := fakeR2(t)

	err := r2.Put(context.Background(), "files/cv", []byte("data"), media.BlobMetadata{
		ContentType:        "application/octet-stream",
		ContentDisposition: "attachment; filename=cv.docx",
	})
	if err != nil {
		t.Fatal(err)
	}
	writes := fake.writes()
	if len(writes) != 1 || writes[0].path != "/media/files/cv" {
		t.Fatalf("writes %+v", writes)
	}
	if got := writes[0].header.Get("Content-Type"); got != "application/octet-stream" {
		t.Fatalf("Content-Type %q", got)
	}
	if got := writes[0].header.Get("Content-Disposition"); got != "attachment; filename=cv.docx" {
		t.Fatalf("Content-Disposition %q", got)
	}
}

func TestR2_SetMetadataReplacesTheMetadataOfTheObjectInPlace(t *testing.T) {
	t.Parallel()
	r2, fake := fakeR2(t)

	err := r2.SetMetadata(context.Background(), "files/page", media.BlobMetadata{
		ContentType:        "application/octet-stream",
		ContentDisposition: "attachment; filename=page.html",
	})
	if err != nil {
		t.Fatal(err)
	}
	writes := fake.writes()
	if len(writes) != 1 || writes[0].path != "/media/files/page" {
		t.Fatalf("writes %+v", writes)
	}
	for header, want := range map[string]string{
		"X-Amz-Copy-Source":        "media/files/page",
		"X-Amz-Metadata-Directive": "REPLACE",
		"Content-Type":             "application/octet-stream",
		"Content-Disposition":      "attachment; filename=page.html",
	} {
		if got := writes[0].header.Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}
}

func TestR2_SetMetadataReportsAMissingObjectAsNotFound(t *testing.T) {
	t.Parallel()
	r2, fake := fakeR2(t)
	fake.fail("/media/files/gone", "NoSuchKey")

	err := r2.SetMetadata(context.Background(), "files/gone", media.BlobMetadata{ContentType: "application/octet-stream"})
	if !errors.Is(err, media.ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
}

func TestR2_SetMetadataDoesNotMistakeAMissingBucketForAMissingObject(t *testing.T) {
	t.Parallel()
	r2, fake := fakeR2(t)
	fake.fail("/media/files/cv", "NoSuchBucket")

	err := r2.SetMetadata(context.Background(), "files/cv", media.BlobMetadata{ContentType: "application/octet-stream"})
	if err == nil || errors.Is(err, media.ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
}

func TestR2_ReadOfAMissingObjectIsNotFound(t *testing.T) {
	t.Parallel()
	r2, fake := fakeR2(t)
	fake.fail("/media/images/gone", "NoSuchKey")

	if _, err := r2.Read(context.Background(), "images/gone"); !errors.Is(err, media.ErrNotFound) {
		t.Fatalf("err = %v, want %v", err, media.ErrNotFound)
	}
}
