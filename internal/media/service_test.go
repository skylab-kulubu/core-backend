package media_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/media"
)

func pngDot() []byte {
	b, err := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==")
	if err != nil {
		panic(err)
	}
	return b
}

func setup(t *testing.T) (media.Service, *media.MemoryBlob) {
	t.Helper()
	blobs := media.NewMemoryBlob()
	svc := media.NewService(media.NewMemoryStore(), blobs, authz.NewAuthorizer(authz.DefaultPolicy()), "https://cdn.example.test")
	return svc, blobs
}

func TestService_UploadImageThenGet(t *testing.T) {
	t.Parallel()
	svc, blobs := setup(t)
	userID := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	p := authz.Principal{ID: userID.String()}

	created, err := svc.Upload(context.Background(), p, "dot.png", "image/png", pngDot())
	if err != nil {
		t.Fatal(err)
	}
	if created.UploadedBy != userID || created.Kind != media.KindImage || created.Type != "image/png" {
		t.Fatalf("created %+v", created)
	}
	if !strings.HasPrefix(created.URL, "https://cdn.example.test/images/") {
		t.Fatalf("url %s", created.URL)
	}
	if _, ok := blobs.Get(created.Key); !ok {
		t.Fatal("blob missing")
	}

	got, err := svc.Get(context.Background(), created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != created.ID || got.URL != created.URL {
		t.Fatalf("got %+v", got)
	}
}

func TestService_AnonymousCannotUpload(t *testing.T) {
	t.Parallel()
	svc, _ := setup(t)
	_, err := svc.Upload(context.Background(), authz.Principal{}, "dot.png", "image/png", pngDot())
	if !errors.Is(err, media.ErrForbidden) {
		t.Fatalf("got %v", err)
	}
}

func TestService_RejectsUnknownImage(t *testing.T) {
	t.Parallel()
	svc, _ := setup(t)
	p := authz.Principal{ID: uuid.MustParse("22222222-2222-2222-2222-222222222222").String()}
	_, err := svc.Upload(context.Background(), p, "x.png", "image/png", []byte("nope"))
	if !errors.Is(err, media.ErrInvalid) {
		t.Fatalf("got %v", err)
	}
}

func TestService_FileNeedsExtension(t *testing.T) {
	t.Parallel()
	svc, _ := setup(t)
	p := authz.Principal{ID: uuid.MustParse("33333333-3333-3333-3333-333333333333").String()}
	_, err := svc.Upload(context.Background(), p, "notes", "application/pdf", []byte("%PDF-1.4"))
	if !errors.Is(err, media.ErrInvalid) {
		t.Fatalf("got %v", err)
	}
	created, err := svc.Upload(context.Background(), p, "notes.pdf", "application/pdf", []byte("%PDF-1.4"))
	if err != nil {
		t.Fatal(err)
	}
	if created.Kind != media.KindFile || !strings.Contains(created.URL, "/files/") {
		t.Fatalf("created %+v", created)
	}
}

func TestService_DropsJPEGMetadataOnUpload(t *testing.T) {
	t.Parallel()
	svc, blobs := setup(t)
	p := authz.Principal{ID: uuid.MustParse("44444444-4444-4444-4444-444444444444").String()}
	in := []byte{0xFF, 0xD8, 0xFF, 0xE1, 0x00, 0x06, 0x45, 0x78, 0x00, 0x00, 0xFF, 0xD9}
	created, err := svc.Upload(context.Background(), p, "x.jpg", "image/jpeg", in)
	if err != nil {
		t.Fatal(err)
	}
	stored, ok := blobs.Get(created.Key)
	if !ok {
		t.Fatal("blob missing")
	}
	if bytes.Contains(stored, []byte{0xFF, 0xE1}) {
		t.Fatalf("exif remained %x", stored)
	}
}

func TestService_ListRequiresAuthAndDeleteIsPrivileged(t *testing.T) {
	t.Parallel()
	svc, blobs := setup(t)
	userID := uuid.MustParse("55555555-5555-5555-5555-555555555555")
	member := authz.Principal{ID: userID.String(), Groups: []string{"/UYELER/ARGE/WEBLAB"}}
	created, err := svc.Upload(context.Background(), member, "dot.png", "image/png", pngDot())
	if err != nil {
		t.Fatal(err)
	}
	_, err = svc.List(context.Background(), authz.Principal{})
	if !errors.Is(err, media.ErrForbidden) {
		t.Fatalf("anon list %v", err)
	}
	_, err = svc.List(context.Background(), member)
	if !errors.Is(err, media.ErrForbidden) {
		t.Fatalf("member list %v", err)
	}
	yk := authz.Principal{ID: "yk", Groups: []string{"/UYELER/YK"}}
	listed, err := svc.List(context.Background(), yk)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].ID != created.ID {
		t.Fatalf("listed %+v", listed)
	}
	err = svc.Delete(context.Background(), member, created.ID)
	if !errors.Is(err, media.ErrForbidden) {
		t.Fatalf("member delete %v", err)
	}
	if err := svc.Delete(context.Background(), yk, created.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := blobs.Get(created.Key); ok {
		t.Fatal("blob remained")
	}
	_, err = svc.Get(context.Background(), created.ID)
	if !errors.Is(err, media.ErrNotFound) {
		t.Fatalf("get after delete %v", err)
	}
}
