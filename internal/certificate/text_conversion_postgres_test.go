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
	"github.com/skylab-kulubu/core-backend/internal/migrate"
	"github.com/skylab-kulubu/core-backend/internal/testpostgres"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
	"github.com/skylab-kulubu/core-backend/internal/transit"
	"github.com/skylab-kulubu/core-backend/internal/transit/transittest"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// A conversion's new background is a pending certificate asset until the
// draft that applies it is saved; then it is attached, and the original
// stays attached through the draft's backgroundOriginalMediaId, which the
// template is found by too. Going back to the original detaches the new
// one (its 30-day window starts) and keeps the original.
func TestPostgresConvertedBackgroundLinks(t *testing.T) {
	pool := testpostgres.Start(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	editorID := uuid.New()
	if _, _, err := user.NewService(user.NewPostgresStore(pool)).Ensure(ctx, editorID, user.Profile{
		Email: "text-editor@example.com", FirstName: "Text", LastName: "Editor", Username: "text-editor",
	}); err != nil {
		t.Fatal(err)
	}
	editor := authz.Principal{ID: editorID.String(), Groups: []string{"/UYELER/YK"}}
	az := authz.NewAuthorizer(authz.DefaultPolicy())
	bao := transittest.NewServer(t)
	mediaStore := media.NewPostgresStore(pool)
	public, private := media.NewMemoryBlob(), media.NewMemoryBlob()
	storage := media.NewPrivateStorage(private, transit.New(bao.Config()))
	catalogue, err := media.LoadCatalogue()
	if err != nil {
		t.Fatal(err)
	}
	mediaSvc := media.NewServiceWithOptions(mediaStore, public, az, "https://cdn.example.test", media.ServiceOptions{
		Catalogue: catalogue,
		Private: &media.PrivateMedia{
			Storage: storage, LinkKey: bytes.Repeat([]byte{7}, 32), LinkOrigin: "https://api.example.test/", AccessLog: mediaStore, Now: bao.Clock.Now,
		},
	})
	tools := &textToolsContract{inspect: certificateInspect(), stripped: []byte("%PDF-1.7 stripped")}
	srv := tools.server(t)
	store := certificate.NewPostgresStore(pool)
	service := certificate.NewServiceWithOptions(store, ticket.NewMemoryStore(), event.NewMemoryStore(), user.NewMemoryStore(), az, staticRenderer{}, nil, certificate.Options{
		Templates: store, Artifacts: public, PrivateArtifacts: storage, Media: media.NewLinker(mediaStore),
		Assets: certificate.MediaAssets{Media: mediaStore, Blobs: public}, Uploads: mediaSvc,
		PDFTools: &certificate.PDFToolsClient{BaseURL: srv.URL, HTTP: srv.Client()},
	})
	original, err := mediaSvc.UploadForPurpose(ctx, editor, media.PurposeCertificateAsset, media.UploadedFile{Name: "canva.pdf", ContentType: "application/pdf", Data: onePagePDF()})
	if err != nil {
		t.Fatal(err)
	}
	layout := certificate.Layout{
		Width: 1123, Height: 794, Orientation: "landscape", BackgroundMediaID: &original.ID,
		Elements: []certificate.Element{
			{ID: "name", Kind: "recipientName", X: 100, Y: 300, Width: 900, Height: 80, FontSize: 48},
			{ID: "qr", Kind: "verificationQr", X: 950, Y: 620, Width: 130, Height: 130},
		},
	}
	template, err := service.CreateTemplate(ctx, editor, certificate.TemplateDraft{Name: "Canva", OwnerTeam: "ARTLAB", SourceKind: "upload", Layout: layout})
	if err != nil {
		t.Fatal(err)
	}
	conversion, err := service.ConvertAssetText(ctx, editor, original.ID, conversionRequest("3"))
	if err != nil {
		t.Fatal(err)
	}
	status := func(id uuid.UUID) (media.Status, bool) {
		t.Helper()
		m, err := mediaStore.Get(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return m.Status, m.ExpiresAt != nil
	}
	if s, expires := status(conversion.BackgroundMediaID); s != media.StatusPending || !expires {
		t.Fatalf("new background before the draft is saved: %s, expires %v", s, expires)
	}

	converted := layout
	converted.BackgroundMediaID = &conversion.BackgroundMediaID
	converted.BackgroundOriginalMediaID = &conversion.BackgroundOriginalMediaID
	converted.Elements = append(append([]certificate.Element{}, layout.Elements...), conversion.Elements...)
	draft := certificate.TemplateDraft{Name: "Canva", OwnerTeam: "ARTLAB", SourceKind: "upload", Layout: converted}
	if _, err := service.UpdateTemplate(ctx, editor, template.ID, draft); err != nil {
		t.Fatalf("save the converted draft: %v", err)
	}
	for name, id := range map[string]uuid.UUID{"new background": conversion.BackgroundMediaID, "original": original.ID} {
		if s, expires := status(id); s != media.StatusAttached || expires {
			t.Fatalf("%s after the draft is saved: %s, expires %v", name, s, expires)
		}
	}
	if using, err := store.TemplatesUsingMedia(ctx, original.ID); err != nil || len(using) != 1 || using[0].ID != template.ID {
		t.Fatalf("templates using the original: %v %+v", err, using)
	}

	// Back to the original.
	draft.Layout = layout
	if _, err := service.UpdateTemplate(ctx, editor, template.ID, draft); err != nil {
		t.Fatal(err)
	}
	if s, expires := status(conversion.BackgroundMediaID); s != media.StatusDetached || !expires {
		t.Fatalf("new background after going back: %s, expires %v", s, expires)
	}
	if s, _ := status(original.ID); s != media.StatusAttached {
		t.Fatalf("original after going back: %s", s)
	}
}
