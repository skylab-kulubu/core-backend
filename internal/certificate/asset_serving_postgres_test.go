package certificate_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/certificate"
	"github.com/skylab-kulubu/core-backend/internal/media"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
	"github.com/skylab-kulubu/core-backend/internal/testpostgres"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

func TestPostgresAssetServingBackfillRewritesOnlyVersionsPublishedBeforeThePolicy(t *testing.T) {
	pool := testpostgres.Start(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	owner := uuid.New()
	if _, _, err := user.NewService(user.NewPostgresStore(pool)).Ensure(ctx, owner, user.Profile{
		Email: "template-owner@example.com", FirstName: "Template", LastName: "Owner", Username: "template-owner",
	}); err != nil {
		t.Fatal(err)
	}
	mediaStore := media.NewPostgresStore(pool)
	logo, err := mediaStore.Create(ctx, media.Media{Name: "logo.svg", Type: "image/svg+xml", Kind: media.KindImage, Key: "images/logo", UploadedBy: owner})
	if err != nil {
		t.Fatal(err)
	}
	store := certificate.NewPostgresStore(pool)
	blobs := media.NewMemoryBlob()
	template, err := store.CreateTemplate(ctx, certificate.Template{ID: uuid.New(), Name: "PG", OwnerTeam: "ARTLAB", SourceKind: "sky", DraftLayout: versionedLayout("PG")})
	if err != nil {
		t.Fatal(err)
	}
	version := func(applied bool) certificate.VersionAssetRef {
		t.Helper()
		id := uuid.New()
		ref := certificate.VersionAssetRef{Key: "certificate-template-assets/" + id.String() + "/" + logo.ID.String(), ContentType: "image/svg+xml"}
		if err := blobs.Put(ctx, ref.Key, []byte("<svg/>"), media.BlobMetadata{ContentType: ref.ContentType}); err != nil {
			t.Fatal(err)
		}
		if _, err := store.CreateVersion(ctx, certificate.TemplateVersion{
			ID: id, TemplateID: template.ID, Layout: template.DraftLayout, Checksum: id.String(),
			AssetManifest: map[string]certificate.VersionAssetRef{logo.ID.String(): ref}, AssetServingPolicyApplied: applied,
		}); err != nil {
			t.Fatal(err)
		}
		return ref
	}
	legacy := version(false)
	current := version(true)
	// Flagging a version must not trip the guard on its layout's Media.
	if err := mediaStore.Archive(ctx, logo.ID, nil); err != nil {
		t.Fatal(err)
	}

	report, err := certificate.BackfillAssetServingPolicy(ctx, store, blobs, nil)
	if err != nil || report.Applied == 0 || report.Failed != 0 {
		t.Fatalf("report %+v err %v", report, err)
	}
	if got, _ := blobs.Metadata(legacy.Key); got != (media.BlobMetadata{ContentType: "image/svg+xml", ContentDisposition: "attachment"}) {
		t.Errorf("legacy copy metadata %+v", got)
	}
	if got, _ := blobs.Metadata(current.Key); got != (media.BlobMetadata{ContentType: "image/svg+xml"}) {
		t.Errorf("a version flagged at publish was rewritten: %+v", got)
	}
	report, err = certificate.BackfillAssetServingPolicy(ctx, store, blobs, nil)
	if err != nil || report != (media.BackfillReport{}) {
		t.Fatalf("second report %+v err %v", report, err)
	}
}
