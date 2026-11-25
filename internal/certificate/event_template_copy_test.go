package certificate_test

import (
	"bytes"
	"context"
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

// An Event copy's template version shares its source version's asset
// copies (docs/certificate-event-templates.md#copy-and-team-change): the
// copy's certificates render the private background through decryption,
// from the source version's encrypted copy, with nothing rendered or copied
// again. The copy is written as the event store's copy writes it: version 1
// with the same layout, asset manifest and checksum, bound to the copy.
func TestEventTemplateCopyRendersFromTheSharedAssetCopies(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	az := authz.NewAuthorizer(authz.DefaultPolicy())
	bao := transittest.NewServer(t)
	mediaStore := media.NewMemoryStore()
	public, private := media.NewMemoryBlob(), media.NewMemoryBlob()
	storage := media.NewPrivateStorage(private, transit.New(bao.Config()))
	uploads := media.NewServiceWithOptions(mediaStore, public, az, "", media.ServiceOptions{
		Private: &media.PrivateMedia{Storage: storage, LinkKey: bytes.Repeat([]byte{1}, 32), AccessLog: mediaStore},
	})
	events := event.NewMemoryStore()
	store := certificate.NewMemoryStore()
	render := &recRender{}
	svc := certificate.NewServiceWithOptions(store, ticket.NewMemoryStore(), events, user.NewMemoryStore(), az, render, nil, certificate.Options{
		Templates: store, Artifacts: public, PrivateArtifacts: storage, Media: media.NewLinker(mediaStore),
		Assets: certificate.MediaAssets{Media: mediaStore, Blobs: public},
	})
	admin := authz.Principal{ID: uuid.NewString(), Groups: []string{"/UYELER/YK"}}
	source, err := events.Create(ctx, event.Event{Name: "Gecekodu 2026", Location: "YTÜ", OwnerTeam: "WEBLAB"})
	if err != nil {
		t.Fatal(err)
	}
	background, err := uploads.UploadForPurpose(ctx, admin, "certificate_asset", media.UploadedFile{Name: "background.png", Data: pngDot()})
	if err != nil {
		t.Fatal(err)
	}
	own, err := svc.CreateEventTemplate(ctx, admin, source.ID, certificate.EventTemplateInput{From: "blank"})
	if err != nil {
		t.Fatal(err)
	}
	draft := own.DraftLayout
	draft.BackgroundMediaID = &background.ID
	if _, err := svc.UpdateTemplate(ctx, admin, own.ID, certificate.TemplateDraft{Name: own.Name, SourceKind: "sky", Layout: draft}); err != nil {
		t.Fatal(err)
	}
	published, err := svc.PublishTemplate(ctx, admin, own.ID)
	if err != nil {
		t.Fatal(err)
	}
	sourceVersion, err := store.GetVersion(ctx, published.ID)
	if err != nil {
		t.Fatal(err)
	}
	if ref := sourceVersion.AssetManifest[background.ID.String()]; ref.Encryption == nil {
		t.Fatalf("source copy not private: %+v", ref)
	}

	// The copy, as the event store writes it.
	copied, err := events.Create(ctx, event.Event{Name: "Gecekodu 2027", Location: "YTÜ", OwnerTeam: "WEBLAB"})
	if err != nil {
		t.Fatal(err)
	}
	copyTemplate, err := store.CreateTemplate(ctx, certificate.Template{
		ID: uuid.New(), Name: "Gecekodu 2027 sertifikası", OwnerTeam: "WEBLAB", EventID: &copied.ID, SourceKind: "sky", DraftLayout: own.DraftLayout,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateVersion(ctx, certificate.TemplateVersion{
		ID: uuid.New(), TemplateID: copyTemplate.ID, Layout: sourceVersion.Layout, AssetManifest: sourceVersion.AssetManifest,
		Checksum: sourceVersion.Checksum, AssetServingPolicyApplied: sourceVersion.AssetServingPolicyApplied,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetBinding(ctx, certificate.Binding{ID: uuid.New(), Scope: certificate.ScopeEvent, ScopeKey: copied.ID.String(), TemplateID: copyTemplate.ID}); err != nil {
		t.Fatal(err)
	}

	resolved, err := svc.ResolveTemplate(ctx, admin, copied.ID)
	if err != nil || resolved.Template.ID != copyTemplate.ID || resolved.Version.Version != 1 || resolved.Version.Checksum != sourceVersion.Checksum {
		t.Fatalf("copy resolves %+v %v", resolved, err)
	}
	render.html = ""
	if _, err := svc.PreviewEvent(ctx, admin, copied.ID); err != nil {
		t.Fatal(err)
	}
	if !embedsTheDot(render.html) {
		t.Fatal("the copy did not render the shared, decrypted background")
	}
}
