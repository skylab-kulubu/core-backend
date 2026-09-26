package certificate_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"image/png"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/certificate"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/media"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
	"github.com/skylab-kulubu/core-backend/internal/transit"
	"github.com/skylab-kulubu/core-backend/internal/transit/transittest"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// privateCertificates is a certificate service with private Media on.
type privateCertificates struct {
	mediaStore *media.MemoryStore
	public     *media.MemoryBlob
	private    *media.MemoryBlob
	storage    *media.PrivateStorage
	uploads    media.Service
	service    certificate.Service
	render     *recRender
	admin      authz.Principal
}

func newPrivateCertificates(t *testing.T) privateCertificates {
	t.Helper()
	az := authz.NewAuthorizer(authz.DefaultPolicy())
	bao := transittest.NewServer(t)
	mediaStore := media.NewMemoryStore()
	public, private := media.NewMemoryBlob(), media.NewMemoryBlob()
	storage := media.NewPrivateStorage(private, transit.New(bao.Config()))
	uploads := media.NewServiceWithOptions(mediaStore, public, az, "", media.ServiceOptions{
		Private: &media.PrivateMedia{Storage: storage, LinkKey: bytes.Repeat([]byte{1}, 32), AccessLog: mediaStore},
	})
	store := certificate.NewMemoryStore()
	render := &recRender{}
	service := certificate.NewServiceWithOptions(store, ticket.NewMemoryStore(), event.NewMemoryStore(), user.NewMemoryStore(), az, render, nil,
		certificate.Options{
			Templates: store, Artifacts: public, PrivateArtifacts: storage, Media: media.NewLinker(mediaStore),
			Assets: certificate.MediaAssets{Media: mediaStore, Blobs: public},
		})
	return privateCertificates{
		mediaStore: mediaStore, public: public, private: private, storage: storage, uploads: uploads, service: service, render: render,
		admin: authz.Principal{ID: uuid.NewString(), Groups: []string{"/UYELER/YK"}},
	}
}

func (c privateCertificates) template(t *testing.T, background uuid.UUID) certificate.Template {
	t.Helper()
	layout := versionedLayout("PRIVATE")
	layout.BackgroundMediaID = &background
	template, err := c.service.CreateTemplate(context.Background(), c.admin, certificate.TemplateDraft{Name: "WEBLAB", OwnerTeam: "WEBLAB", SourceKind: "upload", Layout: layout})
	if err != nil {
		t.Fatal(err)
	}
	return template
}

// A certificate background uploaded as a private certificate asset renders
// through decryption, in the draft preview and in the published version,
// and its published copy never reaches the public bucket.
func TestPrivateCertificateAssetRendersThroughDecryption(t *testing.T) {
	t.Parallel()
	c := newPrivateCertificates(t)
	ctx := context.Background()
	background, err := c.uploads.UploadForPurpose(ctx, c.admin, "certificate_asset", media.UploadedFile{Name: "background.png", Data: pngDot()})
	if err != nil {
		t.Fatal(err)
	}
	template := c.template(t, background.ID)

	if _, err := c.service.PreviewTemplate(ctx, c.admin, template.ID, certificate.PreviewData{}); err != nil {
		t.Fatal(err)
	}
	if !embedsTheDot(c.render.html) {
		t.Fatal("the draft preview did not render the decrypted background")
	}

	c.render.html = ""
	version, err := c.service.PublishTemplate(ctx, c.admin, template.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !embedsTheDot(c.render.html) {
		t.Fatal("the published version did not render the decrypted background")
	}
	copyKey := "certificate-template-assets/" + version.ID.String() + "/" + background.ID.String()
	if _, ok := c.public.Get(copyKey); ok {
		t.Fatal("the private background was copied to the public bucket")
	}
	sealed, ok := c.private.Get(media.PrivateObjectKey(copyKey))
	if !ok || bytes.Contains(sealed, pngDot()) {
		t.Fatalf("private copy present %v, in plaintext %v", ok, ok && bytes.Contains(sealed, pngDot()))
	}
}

// Certificate backgrounds uploaded before private Media are legacy and
// public. With private Media on they stay so, and render as before.
func TestLegacyCertificateAssetStillRendersWithPrivateMediaOn(t *testing.T) {
	t.Parallel()
	c := newPrivateCertificates(t)
	ctx := context.Background()
	background, err := c.uploads.Upload(ctx, c.admin, "background.png", "image/png", pngDot())
	if err != nil {
		t.Fatal(err)
	}
	template := c.template(t, background.ID)
	encoded := base64.StdEncoding.EncodeToString(pngDot())

	if _, err := c.service.PreviewTemplate(ctx, c.admin, template.ID, certificate.PreviewData{}); err != nil || !strings.Contains(c.render.html, encoded) {
		t.Fatalf("draft preview: %v", err)
	}
	c.render.html = ""
	version, err := c.service.PublishTemplate(ctx, c.admin, template.ID)
	if err != nil || !strings.Contains(c.render.html, encoded) {
		t.Fatalf("published version: %v", err)
	}
	if _, ok := c.public.Get("certificate-template-assets/" + version.ID.String() + "/" + background.ID.String()); !ok {
		t.Fatal("the legacy background's copy is not in the public bucket")
	}
	if got, _ := c.mediaStore.Get(ctx, background.ID); got.Purpose != media.PurposeLegacy || got.Visibility != media.VisibilityPublic {
		t.Fatalf("the legacy background became %s %s", got.Purpose, got.Visibility)
	}
}

// Certificate rendering decrypts only core's own private Media: an Answer
// file named in a layout is never read, whatever let the id in.
func TestCertificateRenderingNeverDecryptsAnotherProductsPrivateMedia(t *testing.T) {
	t.Parallel()
	c := newPrivateCertificates(t)
	ctx := context.Background()
	sealed, err := c.storage.Seal(ctx, media.PrivateObjectKey("files/cv"), []byte("%PDF-1.7\n"))
	if err != nil {
		t.Fatal(err)
	}
	answer, err := c.mediaStore.Create(ctx, media.Media{
		Name: "cv.pdf", Type: "application/pdf", Kind: media.KindFile, Key: sealed.Key, UploadedBy: uuid.New(),
		Purpose: media.PurposeAnswerFile, Visibility: media.VisibilityPrivate, Encryption: &sealed.Encryption,
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = certificate.MediaAssets{Media: c.mediaStore, Blobs: c.public}.ReadAsset(ctx, answer.ID)
	if !errors.Is(err, certificate.ErrNotFound) {
		t.Fatalf("err = %v, want %v", err, certificate.ErrNotFound)
	}
}

type failingRender struct{}

func (failingRender) PDF(context.Context, string) ([]byte, error) {
	return nil, errors.New("gotenberg: unavailable")
}

// A publish that fails removes the private copies it made; they are its
// own, under its new version id.
func TestFailedPublishLeavesNoPrivateCopy(t *testing.T) {
	t.Parallel()
	c := newPrivateCertificates(t)
	ctx := context.Background()
	background, err := c.uploads.UploadForPurpose(ctx, c.admin, "certificate_asset", media.UploadedFile{Name: "background.png", Data: pngDot()})
	if err != nil {
		t.Fatal(err)
	}
	template := c.template(t, background.ID)
	store := certificate.NewMemoryStore()
	if _, err := store.CreateTemplate(ctx, template); err != nil {
		t.Fatal(err)
	}
	failing := certificate.NewServiceWithOptions(store, ticket.NewMemoryStore(), event.NewMemoryStore(), user.NewMemoryStore(),
		authz.NewAuthorizer(authz.DefaultPolicy()), failingRender{}, nil,
		certificate.Options{Templates: store, Artifacts: c.public, PrivateArtifacts: c.storage, Assets: certificate.MediaAssets{Media: c.mediaStore, Blobs: c.public}})
	objectsBefore := c.private.Len()

	if _, err := failing.PublishTemplate(ctx, c.admin, template.ID); err == nil {
		t.Fatal("published without a renderer")
	}
	if got := c.private.Len(); got != objectsBefore {
		t.Fatalf("%d private objects after the failed publish, %d before", got, objectsBefore)
	}
}

// failingSecondRead reads assets through reader and fails the second read.
type failingSecondRead struct {
	reader certificate.AssetReader
	mu     sync.Mutex
	reads  int
}

func (f *failingSecondRead) ReadAsset(ctx context.Context, id uuid.UUID) (certificate.Asset, error) {
	f.mu.Lock()
	f.reads++
	n := f.reads
	f.mu.Unlock()
	if n == 2 {
		return certificate.Asset{}, errors.New("r2: read failed")
	}
	return f.reader.ReadAsset(ctx, id)
}

// A publish whose snapshot fails part way deletes the private copies it
// already made.
func TestPublishFailingPartWayThroughItsAssetsLeavesNoPrivateCopy(t *testing.T) {
	t.Parallel()
	c := newPrivateCertificates(t)
	ctx := context.Background()
	var assets []uuid.UUID
	for _, name := range []string{"background.png", "logo.png"} {
		asset, err := c.uploads.UploadForPurpose(ctx, c.admin, "certificate_asset", media.UploadedFile{Name: name, Data: pngDot()})
		if err != nil {
			t.Fatal(err)
		}
		assets = append(assets, asset.ID)
	}
	layout := versionedLayout("TWO ASSETS")
	layout.BackgroundMediaID = &assets[0]
	layout.Elements = append(layout.Elements, certificate.Element{ID: "logo", Kind: "image", MediaID: &assets[1], X: 10, Y: 100, Width: 80, Height: 80, Opacity: 1})
	store := certificate.NewMemoryStore()
	template, err := store.CreateTemplate(ctx, certificate.Template{ID: uuid.New(), Name: "TWO", OwnerTeam: "WEBLAB", SourceKind: "sky", DraftLayout: layout})
	if err != nil {
		t.Fatal(err)
	}
	service := certificate.NewServiceWithOptions(store, ticket.NewMemoryStore(), event.NewMemoryStore(), user.NewMemoryStore(),
		authz.NewAuthorizer(authz.DefaultPolicy()), &recRender{}, nil,
		certificate.Options{Templates: store, Artifacts: c.public, PrivateArtifacts: c.storage,
			Assets: &failingSecondRead{reader: certificate.MediaAssets{Media: c.mediaStore, Blobs: c.public}}})
	objectsBefore := c.private.Len()

	if _, err := service.PublishTemplate(ctx, c.admin, template.ID); err == nil {
		t.Fatal("published although an asset could not be read")
	}
	if got := c.private.Len(); got != objectsBefore {
		t.Fatalf("%d private objects after the failed publish, %d before", got, objectsBefore)
	}
}

// uncertainCommit stores the version and then answers an error, as a commit
// whose answer was lost does.
type uncertainCommit struct {
	*certificate.MemoryStore
}

func (u uncertainCommit) CreateVersion(ctx context.Context, v certificate.TemplateVersion) (certificate.TemplateVersion, error) {
	if _, err := u.MemoryStore.CreateVersion(ctx, v); err != nil {
		return certificate.TemplateVersion{}, err
	}
	return certificate.TemplateVersion{}, errors.New("postgres: connection lost during commit")
}

// A publish whose version may have been stored keeps its private copies:
// deleting them would break every certificate of that version.
func TestPublishWithAnUncertainCommitKeepsItsPrivateCopies(t *testing.T) {
	t.Parallel()
	c := newPrivateCertificates(t)
	ctx := context.Background()
	background, err := c.uploads.UploadForPurpose(ctx, c.admin, "certificate_asset", media.UploadedFile{Name: "background.png", Data: pngDot()})
	if err != nil {
		t.Fatal(err)
	}
	layout := versionedLayout("UNCERTAIN")
	layout.BackgroundMediaID = &background.ID
	memory := certificate.NewMemoryStore()
	template, err := memory.CreateTemplate(ctx, certificate.Template{ID: uuid.New(), Name: "UNCERTAIN", OwnerTeam: "WEBLAB", SourceKind: "sky", DraftLayout: layout})
	if err != nil {
		t.Fatal(err)
	}
	store := uncertainCommit{memory}
	service := certificate.NewServiceWithOptions(store, ticket.NewMemoryStore(), event.NewMemoryStore(), user.NewMemoryStore(),
		authz.NewAuthorizer(authz.DefaultPolicy()), &recRender{}, nil,
		certificate.Options{Templates: store, Artifacts: c.public, PrivateArtifacts: c.storage, Assets: certificate.MediaAssets{Media: c.mediaStore, Blobs: c.public}})
	objectsBefore := c.private.Len()

	if _, err := service.PublishTemplate(ctx, c.admin, template.ID); err == nil {
		t.Fatal("the lost commit answer was not reported")
	}
	version, err := memory.LatestVersion(ctx, template.ID)
	if err != nil {
		t.Fatal(err)
	}
	ref := version.AssetManifest[background.ID.String()]
	if _, ok := c.private.Get(ref.Key); !ok || c.private.Len() != objectsBefore+1 {
		t.Fatalf("the stored version's private copy is gone (%d objects, %d before)", c.private.Len(), objectsBefore)
	}
}

// embedsTheDot reports whether the rendered HTML embeds the 1-pixel PNG
// background: decrypted, as it was stored (re-encoded, still 1 by 1).
func embedsTheDot(html string) bool {
	const prefix = "data:image/png;base64,"
	start := strings.Index(html, prefix)
	if start < 0 {
		return false
	}
	rest := html[start+len(prefix):]
	end := strings.IndexAny(rest, "\"')")
	if end < 0 {
		return false
	}
	data, err := base64.StdEncoding.DecodeString(rest[:end])
	if err != nil {
		return false
	}
	config, err := png.DecodeConfig(bytes.NewReader(data))
	return err == nil && config.Width == 1 && config.Height == 1
}
