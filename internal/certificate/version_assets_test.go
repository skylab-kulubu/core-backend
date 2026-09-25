package certificate_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/certificate"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/media"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

type layoutAssets map[uuid.UUID]certificate.Asset

func (a layoutAssets) ReadAsset(_ context.Context, id uuid.UUID) (certificate.Asset, error) {
	asset, ok := a[id]
	if !ok {
		return certificate.Asset{}, certificate.ErrNotFound
	}
	return asset, nil
}

type publishedAssets struct {
	store     *certificate.MemoryStore
	artifacts *media.MemoryBlob
	version   certificate.TemplateVersion
	logoID    uuid.UUID
	photoID   uuid.UUID
}

// publishWithAssets publishes a template whose layout places an SVG and a
// PNG Media.
func publishWithAssets(t *testing.T) publishedAssets {
	t.Helper()
	ctx := context.Background()
	store := certificate.NewMemoryStore()
	artifacts := media.NewMemoryBlob()
	logoID, photoID := uuid.New(), uuid.New()
	assets := layoutAssets{
		logoID:  {ContentType: "image/svg+xml", Data: []byte(`<svg xmlns="http://www.w3.org/2000/svg"><rect width="1" height="1"/></svg>`)},
		photoID: {ContentType: "image/png", Data: []byte("png")},
	}
	service := certificate.NewServiceWithOptions(store, ticket.NewMemoryStore(), event.NewMemoryStore(), user.NewMemoryStore(),
		authz.NewAuthorizer(authz.DefaultPolicy()), &recRender{}, nil,
		certificate.Options{Templates: store, Artifacts: artifacts, Assets: assets})
	layout := versionedLayout("ASSETS")
	layout.Elements = append(layout.Elements,
		certificate.Element{ID: "logo", Kind: "image", MediaID: &logoID, X: 10, Y: 100, Width: 80, Height: 80, Opacity: 1},
		certificate.Element{ID: "photo", Kind: "image", MediaID: &photoID, X: 10, Y: 400, Width: 80, Height: 80, Opacity: 1},
	)
	template, err := store.CreateTemplate(ctx, certificate.Template{ID: uuid.New(), Name: "ASSETS", OwnerTeam: "ARTLAB", SourceKind: "sky", DraftLayout: layout})
	if err != nil {
		t.Fatal(err)
	}
	version, err := service.PublishTemplate(ctx, authz.Principal{ID: uuid.NewString(), Groups: []string{"/ADMIN"}}, template.ID)
	if err != nil {
		t.Fatal(err)
	}
	return publishedAssets{store: store, artifacts: artifacts, version: version, logoID: logoID, photoID: photoID}
}

func TestPublishedTemplateAssetsFollowTheMediaServingPolicy(t *testing.T) {
	t.Parallel()
	published := publishWithAssets(t)

	prefix := "certificate-template-assets/" + published.version.ID.String() + "/"
	if got, _ := published.artifacts.Metadata(prefix + published.logoID.String()); got != (media.BlobMetadata{ContentType: "image/svg+xml", ContentDisposition: "attachment"}) {
		t.Errorf("svg asset metadata %+v", got)
	}
	if got, _ := published.artifacts.Metadata(prefix + published.photoID.String()); got != (media.BlobMetadata{ContentType: "image/png"}) {
		t.Errorf("png asset metadata %+v", got)
	}
}

func TestAssetServingBackfillSkipsVersionsPublishedUnderThePolicy(t *testing.T) {
	t.Parallel()
	published := publishWithAssets(t)

	report, err := certificate.BackfillAssetServingPolicy(context.Background(), published.store, published.artifacts, nil)
	if err != nil || report != (media.BackfillReport{}) {
		t.Fatalf("report %+v err %v", report, err)
	}
}

func TestAssetServingBackfillBringsExistingTemplateAssetsUnderThePolicy(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := certificate.NewMemoryStore()
	blobs := media.NewMemoryBlob()
	template, err := store.CreateTemplate(ctx, certificate.Template{ID: uuid.New(), Name: "LEGACY", OwnerTeam: "ARTLAB", SourceKind: "sky", DraftLayout: versionedLayout("LEGACY")})
	if err != nil {
		t.Fatal(err)
	}
	versionID := uuid.New()
	logo := certificate.VersionAssetRef{Key: "certificate-template-assets/" + versionID.String() + "/logo", ContentType: "image/svg+xml"}
	photo := certificate.VersionAssetRef{Key: "certificate-template-assets/" + versionID.String() + "/photo", ContentType: "image/png"}
	gone := certificate.VersionAssetRef{Key: "certificate-template-assets/" + versionID.String() + "/gone", ContentType: "image/svg+xml"}
	for _, ref := range []certificate.VersionAssetRef{logo, photo} {
		if err := blobs.Put(ctx, ref.Key, []byte("legacy"), media.BlobMetadata{ContentType: ref.ContentType}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.CreateVersion(ctx, certificate.TemplateVersion{
		ID: versionID, TemplateID: template.ID, Layout: template.DraftLayout, Checksum: "legacy",
		AssetManifest: map[string]certificate.VersionAssetRef{"logo": logo, "photo": photo, "gone": gone},
	}); err != nil {
		t.Fatal(err)
	}

	report, err := certificate.BackfillAssetServingPolicy(ctx, store, blobs, nil)
	if err != nil || report != (media.BackfillReport{Applied: 1}) {
		t.Fatalf("report %+v err %v", report, err)
	}
	if got, _ := blobs.Metadata(logo.Key); got != (media.BlobMetadata{ContentType: "image/svg+xml", ContentDisposition: "attachment"}) {
		t.Errorf("svg asset metadata %+v", got)
	}
	if got, _ := blobs.Metadata(photo.Key); got != (media.BlobMetadata{ContentType: "image/png"}) {
		t.Errorf("png asset metadata %+v", got)
	}
	report, err = certificate.BackfillAssetServingPolicy(ctx, store, blobs, nil)
	if err != nil || report != (media.BackfillReport{}) {
		t.Fatalf("second report %+v err %v", report, err)
	}
}
