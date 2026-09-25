package media_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"mime"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/lifecycle"
	"github.com/skylab-kulubu/core-backend/internal/media"
)

type rejectingCreateStore struct {
	*media.MemoryStore
}

type uncertainStagingStore struct {
	*media.MemoryStore
	readyCalls int
}

func (*uncertainStagingStore) StageUpload(context.Context, string, uuid.UUID, time.Time) error {
	return nil
}
func (*uncertainStagingStore) CreateStaged(context.Context, media.Media) (media.Media, error) {
	return media.Media{}, media.ErrPublicationUncertain
}
func (s *uncertainStagingStore) ReadyStagedUploadForCleanup(context.Context, string, time.Time) error {
	s.readyCalls++
	return nil
}
func (*uncertainStagingStore) CancelStagedUpload(context.Context, string) error { return nil }
func (*uncertainStagingStore) PurgeNextStagedUpload(context.Context, time.Time, func(string) error) (bool, error) {
	return false, nil
}

func (rejectingCreateStore) Create(context.Context, media.Media) (media.Media, error) {
	return media.Media{}, media.ErrForbidden
}

type recordingBlobStore struct {
	objects   map[string][]byte
	deleted   []string
	deleteErr error
}

func (b *recordingBlobStore) Put(_ context.Context, key string, data []byte, _ media.BlobMetadata) error {
	if b.objects == nil {
		b.objects = make(map[string][]byte)
	}
	b.objects[key] = append([]byte(nil), data...)
	return nil
}

func (b *recordingBlobStore) SetMetadata(context.Context, string, media.BlobMetadata) error {
	return nil
}

func (b *recordingBlobStore) Read(_ context.Context, key string) ([]byte, error) {
	data, ok := b.objects[key]
	if !ok {
		return nil, media.ErrNotFound
	}
	return append([]byte(nil), data...), nil
}

func (b *recordingBlobStore) Delete(_ context.Context, key string) error {
	if b.deleteErr != nil {
		return b.deleteErr
	}
	delete(b.objects, key)
	b.deleted = append(b.deleted, key)
	return nil
}

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

func TestService_UploadDeletesBlobWhenMetadataCreateIsRejected(t *testing.T) {
	t.Parallel()
	blobs := &recordingBlobStore{}
	svc := media.NewService(
		rejectingCreateStore{MemoryStore: media.NewMemoryStore()},
		blobs,
		authz.NewAuthorizer(authz.DefaultPolicy()),
		"https://cdn.example.test",
	)
	p := authz.Principal{ID: uuid.MustParse("13131313-1313-1313-1313-131313131313").String()}

	_, err := svc.Upload(context.Background(), p, "rejected.png", "image/png", pngDot())
	if !errors.Is(err, media.ErrForbidden) {
		t.Fatalf("upload error = %v", err)
	}
	if len(blobs.deleted) != 1 {
		t.Fatalf("deleted keys = %v", blobs.deleted)
	}
	if len(blobs.objects) != 0 {
		t.Fatalf("metadata rejection left %d orphan blobs", len(blobs.objects))
	}
}

func TestService_DoesNotDeleteBlobWhenMetadataCommitOutcomeIsUncertain(t *testing.T) {
	t.Parallel()
	store := &uncertainStagingStore{MemoryStore: media.NewMemoryStore()}
	blobs := &recordingBlobStore{}
	svc := media.NewService(store, blobs, authz.NewAuthorizer(authz.DefaultPolicy()), "")
	p := authz.Principal{ID: uuid.MustParse("14141414-1414-1414-1414-141414141414").String()}

	_, err := svc.Upload(context.Background(), p, "uncertain.png", "image/png", pngDot())
	if !errors.Is(err, media.ErrPublicationUncertain) {
		t.Fatalf("upload error = %v", err)
	}
	if len(blobs.objects) != 1 || len(blobs.deleted) != 0 || store.readyCalls != 0 {
		t.Fatalf("uncertain publication cleanup objects=%d deleted=%v ready=%d", len(blobs.objects), blobs.deleted, store.readyCalls)
	}
}

func TestService_UploadComputesAndStoresCoverColors(t *testing.T) {
	t.Parallel()
	store := media.NewMemoryStore()
	blobs := media.NewMemoryBlob()
	svc := media.NewService(store, blobs, authz.NewAuthorizer(authz.DefaultPolicy()), "https://cdn.example.test")
	p := authz.Principal{ID: uuid.MustParse("12121212-1212-1212-1212-121212121212").String()}

	created, err := svc.Upload(context.Background(), p, "cover.png", "image/png", twoTonePNG(t))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"#3c82be", "#8a642f"}
	if !reflect.DeepEqual(created.CoverColors, want) || !created.CoverColorsComputed {
		t.Fatalf("created colors %#v computed %v", created.CoverColors, created.CoverColorsComputed)
	}
	stored, err := store.Get(context.Background(), created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(stored.CoverColors, want) || !stored.CoverColorsComputed {
		t.Fatalf("stored colors %#v computed %v", stored.CoverColors, stored.CoverColorsComputed)
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

func TestService_RejectsSpoofedPDF(t *testing.T) {
	t.Parallel()
	svc, _ := setup(t)
	p := authz.Principal{ID: uuid.MustParse("34343434-3434-3434-3434-343434343434").String()}
	_, err := svc.Upload(context.Background(), p, "certificate.pdf", "application/pdf", []byte("not a pdf"))
	if !errors.Is(err, media.ErrInvalid) {
		t.Fatalf("got %v", err)
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

func TestService_ArchiveOwnOnlyByUploader(t *testing.T) {
	t.Parallel()
	store := media.NewMemoryStore()
	blobs := media.NewMemoryBlob()
	svc := media.NewService(store, blobs, authz.NewAuthorizer(authz.DefaultPolicy()), "https://cdn.example.test")
	uploaderID := uuid.MustParse("66666666-6666-6666-6666-666666666666")
	uploader := authz.Principal{ID: uploaderID.String(), Groups: []string{"/UYELER/ARGE/WEBLAB"}}
	created, err := svc.Upload(context.Background(), uploader, "me.png", "image/png", pngDot())
	if err != nil {
		t.Fatal(err)
	}

	other := authz.Principal{ID: uuid.MustParse("77777777-7777-7777-7777-777777777777").String(), Groups: []string{"/UYELER/ARGE/WEBLAB"}}
	if err := svc.ArchiveOwn(context.Background(), other, created.ID); !errors.Is(err, media.ErrForbidden) {
		t.Fatalf("other member archive %v", err)
	}
	if err := svc.ArchiveOwn(context.Background(), authz.Principal{}, created.ID); !errors.Is(err, media.ErrInvalid) {
		t.Fatalf("anonymous archive %v", err)
	}
	if _, err := svc.Get(context.Background(), created.ID); err != nil {
		t.Fatalf("rejected archive changed the record: %v", err)
	}
	if err := svc.ArchiveOwn(context.Background(), uploader, uuid.New()); !errors.Is(err, media.ErrNotFound) {
		t.Fatalf("unknown media %v", err)
	}

	if err := svc.ArchiveOwn(context.Background(), uploader, created.ID); err != nil {
		t.Fatal(err)
	}
	if err := svc.ArchiveOwn(context.Background(), uploader, created.ID); err != nil {
		t.Fatalf("repeated archive: %v", err)
	}
	if _, ok := blobs.Get(created.Key); !ok {
		t.Fatal("archive removed blob before recovery window")
	}
	if _, err := svc.Get(context.Background(), created.ID); !errors.Is(err, media.ErrNotFound) {
		t.Fatalf("get after archive %v", err)
	}
	yk := authz.Principal{ID: uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa").String(), Groups: []string{"/UYELER/YK"}}
	archived, err := svc.ListLifecycle(context.Background(), yk, lifecycle.InactiveOnly)
	if err != nil {
		t.Fatal(err)
	}
	if len(archived) != 1 || archived[0].ID != created.ID || archived[0].DeletedAt == nil || archived[0].DeletedBy == nil || *archived[0].DeletedBy != uploaderID {
		t.Fatalf("archived %+v", archived)
	}
	if _, err := svc.Restore(context.Background(), yk, created.ID); err != nil {
		t.Fatalf("management restore after self archive: %v", err)
	}
}

func TestService_ListRequiresAuthAndDeleteIsPrivileged(t *testing.T) {
	t.Parallel()
	store := media.NewMemoryStore()
	blobs := media.NewMemoryBlob()
	svc := media.NewService(store, blobs, authz.NewAuthorizer(authz.DefaultPolicy()), "https://cdn.example.test")
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
	operatorID := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	yk := authz.Principal{ID: operatorID.String(), Groups: []string{"/UYELER/YK"}}
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
	if err := svc.Delete(context.Background(), yk, created.ID); err != nil {
		t.Fatalf("repeated delete: %v", err)
	}
	if _, ok := blobs.Get(created.Key); !ok {
		t.Fatal("archive removed blob before recovery window")
	}
	_, err = svc.Get(context.Background(), created.ID)
	if !errors.Is(err, media.ErrNotFound) {
		t.Fatalf("get after delete %v", err)
	}
	archived, err := svc.ListLifecycle(context.Background(), yk, lifecycle.InactiveOnly)
	if err != nil {
		t.Fatal(err)
	}
	if len(archived) != 1 || archived[0].ID != created.ID || archived[0].DeletedAt == nil || archived[0].DeletedBy == nil || *archived[0].DeletedBy != operatorID {
		t.Fatalf("archived %+v", archived)
	}
	if _, err := svc.ListLifecycle(context.Background(), member, lifecycle.InactiveOnly); !errors.Is(err, media.ErrForbidden) {
		t.Fatalf("member lifecycle list %v", err)
	}

	restored, err := svc.Restore(context.Background(), yk, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Restore(context.Background(), yk, created.ID); err != nil {
		t.Fatalf("repeated restore: %v", err)
	}
	if restored.DeletedAt != nil || restored.DeletedBy != nil || restored.ID != created.ID {
		t.Fatalf("restored %+v", restored)
	}
	if _, err := svc.Get(context.Background(), created.ID); err != nil {
		t.Fatalf("get restored: %v", err)
	}
}

func TestService_UploadServesOtherFilesAsNamedDownloads(t *testing.T) {
	t.Parallel()
	svc, blobs := setup(t)
	p := authz.Principal{ID: uuid.MustParse("35353535-3535-3535-3535-353535353535").String()}

	created, err := svc.Upload(context.Background(), p, "Özgeçmiş.html", "text/html", []byte("<html><script>alert(1)</script></html>"))
	if err != nil {
		t.Fatal(err)
	}
	got, ok := blobs.Metadata(created.Key)
	want := media.BlobMetadata{
		ContentType:        "application/octet-stream",
		ContentDisposition: "attachment; filename*=utf-8''%C3%96zge%C3%A7mi%C5%9F.html",
	}
	if !ok || got != want {
		t.Fatalf("blob metadata = %+v (stored %v), want %+v", got, ok, want)
	}
}

func TestService_UploadKeepsHostileFileNamesInsideTheDispositionParameter(t *testing.T) {
	t.Parallel()
	svc, blobs := setup(t)
	p := authz.Principal{ID: uuid.MustParse("36363636-3636-3636-3636-363636363636").String()}
	name := "cv\"; filename=\"x.html\r\nContent-Type: text/html; a=b.txt"

	created, err := svc.Upload(context.Background(), p, name, "text/plain", []byte("hello"))
	if err != nil {
		t.Fatal(err)
	}
	got, _ := blobs.Metadata(created.Key)
	if strings.ContainsAny(got.ContentDisposition, "\r\n") {
		t.Fatalf("disposition carries a line break: %q", got.ContentDisposition)
	}
	disposition, params, err := mime.ParseMediaType(got.ContentDisposition)
	if err != nil || disposition != "attachment" || len(params) != 1 || params["filename"] != name {
		t.Fatalf("disposition %q parsed as %q %v (%v)", got.ContentDisposition, disposition, params, err)
	}
}

func TestService_UploadRecordsTheFilesOwnType(t *testing.T) {
	t.Parallel()
	svc, _ := setup(t)
	p := authz.Principal{ID: uuid.MustParse("37373737-3737-3737-3737-373737373737").String()}

	created, err := svc.Upload(context.Background(), p, "page.html", "text/html", []byte("<html></html>"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := svc.Get(context.Background(), created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if created.Type != "text/html" || got.Type != "text/html" {
		t.Fatalf("created type %q, stored type %q", created.Type, got.Type)
	}
}

func TestService_UploadServesSVGAsAnImageThatDownloadsWhenOpened(t *testing.T) {
	t.Parallel()
	svc, blobs := setup(t)
	p := authz.Principal{ID: uuid.MustParse("38383838-3838-3838-3838-383838383838").String()}

	created, err := svc.Upload(context.Background(), p, "logo.svg", "image/svg+xml", []byte(`<svg xmlns="http://www.w3.org/2000/svg"><rect width="1" height="1"/></svg>`))
	if err != nil {
		t.Fatal(err)
	}
	got, _ := blobs.Metadata(created.Key)
	want := media.BlobMetadata{ContentType: "image/svg+xml", ContentDisposition: "attachment; filename=logo.svg"}
	if got != want || created.Type != "image/svg+xml" {
		t.Fatalf("blob metadata %+v, record type %q", got, created.Type)
	}
}

func TestService_UploadServesRasterImagesAndPDFsInline(t *testing.T) {
	t.Parallel()
	svc, blobs := setup(t)
	p := authz.Principal{ID: uuid.MustParse("39393939-3939-3939-3939-393939393939").String()}
	for _, tc := range []struct {
		name, declared string
		data           []byte
		want           media.BlobMetadata
	}{
		{"dot.png", "image/png", pngDot(), media.BlobMetadata{ContentType: "image/png"}},
		{"cv.pdf", "application/pdf", []byte("%PDF-1.4"), media.BlobMetadata{ContentType: "application/pdf"}},
	} {
		created, err := svc.Upload(context.Background(), p, tc.name, tc.declared, tc.data)
		if err != nil {
			t.Fatal(err)
		}
		if got, _ := blobs.Metadata(created.Key); got != tc.want {
			t.Fatalf("%s blob metadata %+v, want %+v", tc.name, got, tc.want)
		}
	}
}

func TestService_UploadWithoutPurposeIsLegacy(t *testing.T) {
	t.Parallel()
	svc, _ := setup(t)
	p := authz.Principal{ID: uuid.MustParse("40404040-4040-4040-4040-404040404040").String()}

	created, err := svc.Upload(context.Background(), p, "page.html", "text/html", []byte("<html></html>"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := svc.Get(context.Background(), created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if created.Purpose != media.PurposeLegacy || got.Purpose != media.PurposeLegacy {
		t.Fatalf("created purpose %q, stored purpose %q", created.Purpose, got.Purpose)
	}
}
