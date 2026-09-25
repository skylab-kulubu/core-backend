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

func TestPublishedTemplateAssetsFollowTheMediaServingPolicy(t *testing.T) {
	t.Parallel()
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
	prefix := "certificate-template-assets/" + version.ID.String() + "/"
	if got, _ := artifacts.Metadata(prefix + logoID.String()); got != (media.BlobMetadata{ContentType: "image/svg+xml", ContentDisposition: "attachment"}) {
		t.Errorf("svg asset metadata %+v", got)
	}
	if got, _ := artifacts.Metadata(prefix + photoID.String()); got != (media.BlobMetadata{ContentType: "image/png"}) {
		t.Errorf("png asset metadata %+v", got)
	}
}
