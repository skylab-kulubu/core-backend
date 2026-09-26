package media_test

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/media"
)

func TestBlobAndCDNUsesJavaBucketAndPublicURLNames(t *testing.T) {
	t.Parallel()
	env := map[string]string{
		"R2_ENDPOINT":    "https://account.r2.cloudflarestorage.com",
		"R2_BUCKET_NAME": "skylab-cdn",
		"R2_ACCESS_KEY":  "ak",
		"R2_SECRET_KEY":  "sk",
		"R2_PUBLIC_URL":  "https://cdn.yildizskylab.com",
	}
	blob, cdn, err := media.BlobAndCDN(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	if cdn != "https://cdn.yildizskylab.com" {
		t.Fatalf("cdn %s", cdn)
	}
	if _, ok := blob.(*media.R2); !ok {
		t.Fatalf("got %T", blob)
	}
}

func TestBlobAndCDNPrefersR2BucketOverBucketName(t *testing.T) {
	t.Parallel()
	env := map[string]string{
		"R2_ENDPOINT":    "https://account.r2.cloudflarestorage.com",
		"R2_BUCKET":      "go-bucket",
		"R2_BUCKET_NAME": "java-bucket",
		"R2_ACCESS_KEY":  "ak",
		"R2_SECRET_KEY":  "sk",
	}
	blob, _, err := media.BlobAndCDN(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	r2, ok := blob.(*media.R2)
	if !ok {
		t.Fatalf("got %T", blob)
	}
	if r2.Bucket() != "go-bucket" {
		t.Fatalf("bucket %s", r2.Bucket())
	}
}

func TestBlobAndCDNRejectsIncompleteR2(t *testing.T) {
	t.Parallel()
	env := map[string]string{"R2_ENDPOINT": "https://account.r2.cloudflarestorage.com"}
	_, _, err := media.BlobAndCDN(func(k string) string { return env[k] })
	if err == nil {
		t.Fatal("expected error")
	}
}

// Every Media address is built from a base, so a client never gets a bare
// key: without a configured one, the default base, as every other address
// core builds without one.
func TestUploadWithoutCDNBaseGetsTheDefaultBase(t *testing.T) {
	t.Parallel()
	svc := media.NewService(media.NewMemoryStore(), media.NewMemoryBlob(), authz.NewAuthorizer(authz.DefaultPolicy()), "")
	p := authz.Principal{ID: uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa").String()}
	created, err := svc.Upload(context.Background(), p, "dot.png", "image/png", pngDot())
	if err != nil {
		t.Fatal(err)
	}
	if created.URL != media.PublicURL("", created.Key) || !strings.HasPrefix(created.URL, "https://") {
		t.Fatalf("url %s", created.URL)
	}
}

func TestBlobAndCDNMemoryWhenUnset(t *testing.T) {
	t.Parallel()
	blob, cdn, err := media.BlobAndCDN(func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	if cdn != "" {
		t.Fatalf("cdn %s", cdn)
	}
	if _, ok := blob.(*media.MemoryBlob); !ok {
		t.Fatalf("got %T", blob)
	}
}
