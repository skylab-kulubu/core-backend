package certificate_test

import (
	"context"
	"sort"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/certificate"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/media"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
	"github.com/skylab-kulubu/core-backend/internal/testpostgres"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// The placed copy lives in the manifest under the background's own key:
// publishing a placed PDF background links the same Media as publishing
// it unplaced, the database's guards and links read no new id, and the
// template is found by the Media for the asset routes.
func TestPostgresPlacedPublishKeepsTheMediaLinks(t *testing.T) {
	pool := testpostgres.Start(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	adminID := uuid.New()
	if _, _, err := user.NewService(user.NewPostgresStore(pool)).Ensure(ctx, adminID, user.Profile{
		Email: "placed-admin@example.com", FirstName: "Placed", LastName: "Admin", Username: "placed-admin",
	}); err != nil {
		t.Fatal(err)
	}
	admin := authz.Principal{ID: adminID.String(), Groups: []string{"/UYELER/YK"}}
	mediaStore := media.NewPostgresStore(pool)
	blobs := media.NewMemoryBlob()
	if err := blobs.Put(ctx, "files/placed-background.pdf", onePagePDF(), media.ServingMetadata("application/pdf", "")); err != nil {
		t.Fatal(err)
	}
	background, err := mediaStore.Create(ctx, media.Media{Name: "background.pdf", Type: "application/pdf", Kind: media.KindFile, Key: "files/placed-background.pdf", UploadedBy: adminID})
	if err != nil {
		t.Fatal(err)
	}
	gotenberg := (&gotenbergContract{pages: 1}).server(t)
	tools := &pdfToolsContract{widthPt: 595.276, heightPt: 841.89, pages: 1}
	store := certificate.NewPostgresStore(pool)
	service := certificate.NewServiceWithOptions(store, ticket.NewMemoryStore(), event.NewMemoryStore(), user.NewMemoryStore(), authz.NewAuthorizer(authz.DefaultPolicy()),
		&certificate.Gotenberg{BaseURL: gotenberg.URL, HTTP: gotenberg.Client()}, nil, certificate.Options{
			Templates: store, Artifacts: blobs, Media: media.NewLinker(mediaStore),
			Assets: certificate.MediaAssets{Media: mediaStore, Blobs: blobs}, PDFTools: tools.client(t),
		})
	layout := certificate.Layout{
		Width: 1123, Height: 794, Orientation: "landscape", BackgroundMediaID: &background.ID,
		BackgroundPlacement: &certificate.BackgroundPlacement{Fit: certificate.FitContain, X: 0.5, Y: 0.5, Scale: 1},
		Elements: []certificate.Element{
			{ID: "name", Kind: "recipientName", X: 100, Y: 300, Width: 900, Height: 80, FontSize: 48},
			{ID: "qr", Kind: "verificationQr", X: 950, Y: 620, Width: 130, Height: 130},
		},
	}
	template, err := service.CreateTemplate(ctx, admin, certificate.TemplateDraft{Name: "Placed", OwnerTeam: "ARTLAB", SourceKind: "upload", Layout: layout})
	if err != nil {
		t.Fatal(err)
	}
	links := func(ownerType string, owner uuid.UUID) []string {
		t.Helper()
		rows, err := pool.Query(ctx, `SELECT media_id::text FROM media_attachments WHERE owner_service = 'core' AND owner_type = $1 AND owner_id = $2`, ownerType, owner)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				t.Fatal(err)
			}
			out = append(out, id)
		}
		sort.Strings(out)
		return out
	}
	draftBefore := links("certificate_template", template.ID)

	version, err := service.PublishTemplate(ctx, admin, template.ID)
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	stored, err := store.GetVersion(ctx, version.ID)
	if err != nil {
		t.Fatal(err)
	}
	if ref := stored.AssetManifest[background.ID.String()]; ref.Placed == nil || len(stored.AssetManifest) != 1 {
		t.Fatalf("stored manifest %+v", stored.AssetManifest)
	}
	if got := links("certificate_template_version", version.ID); len(got) != 1 || got[0] != background.ID.String() {
		t.Fatalf("version links %v, want only the background %s", got, background.ID)
	}
	if got := links("certificate_template", template.ID); len(got) != len(draftBefore) || (len(got) == 1 && got[0] != draftBefore[0]) {
		t.Fatalf("draft links %v, before %v", got, draftBefore)
	}

	using, err := store.TemplatesUsingMedia(ctx, background.ID)
	if err != nil || len(using) != 1 || using[0].ID != template.ID {
		t.Fatalf("templates using the background: %v %+v", err, using)
	}
	if using, err := store.TemplatesUsingMedia(ctx, uuid.New()); err != nil || len(using) != 0 {
		t.Fatalf("templates using another Media: %v %+v", err, using)
	}
}
