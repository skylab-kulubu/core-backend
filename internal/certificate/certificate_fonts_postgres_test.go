package certificate_test

import (
	"bytes"
	"context"
	"errors"
	"strings"
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

// A certificate font in the database: the upload stays pending (it is not
// the font, and expires), the checked file is attached by the font row; a
// template that sets text in it links the file too and is found by it; the
// same face again is font_exists; archived, the font leaves the list and a
// new version of a draft that has it still prints it.
func TestPostgresCertificateFonts(t *testing.T) {
	pool := testpostgres.Start(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	leaderID := uuid.New()
	if _, _, err := user.NewService(user.NewPostgresStore(pool)).Ensure(ctx, leaderID, user.Profile{
		Email: "font-leader@example.com", FirstName: "Font", LastName: "Leader", Username: "font-leader",
	}); err != nil {
		t.Fatal(err)
	}
	leader := authz.Principal{ID: leaderID.String(), Groups: []string{"/UYELER/ARGE/ARTLAB/LIDERLER"}}
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
	tools := newFontTools()
	srv := tools.server(t)
	store := certificate.NewPostgresStore(pool)
	render := &recRender{}
	service := certificate.NewServiceWithOptions(store, ticket.NewMemoryStore(), event.NewMemoryStore(), user.NewMemoryStore(), az, render, &recMail{}, certificate.Options{
		PublicAPIOrigin: "https://api.example.test", VerifyOrigin: "https://skyl.app/c",
		Templates: store, Artifacts: public, PrivateArtifacts: storage, Media: media.NewLinker(mediaStore),
		Assets: certificate.MediaAssets{Media: mediaStore, Blobs: public}, Uploads: mediaSvc,
		PDFTools: &certificate.PDFToolsClient{BaseURL: srv.URL, HTTP: srv.Client()},
	})
	raw, err := mediaSvc.UploadForPurpose(ctx, leader, media.PurposeCertificateFont, media.UploadedFile{Name: "brandon.otf", Data: []byte("OTTO brandon")})
	if err != nil {
		t.Fatal(err)
	}
	font, err := service.CreateFont(ctx, leader, certificate.FontInput{MediaID: raw.ID, OwnerTeam: "ARTLAB"})
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
	if s, expires := status(raw.ID); s != media.StatusPending || !expires {
		t.Fatalf("upload: %s, expires %v", s, expires)
	}
	if s, expires := status(font.MediaID); s != media.StatusAttached || expires {
		t.Fatalf("checked file: %s, expires %v", s, expires)
	}
	if listed, err := service.ListFonts(ctx, leader, "ARTLAB"); err != nil || len(listed) != 1 || listed[0].ID != font.ID || listed[0].MissingTurkish != "ğı" {
		t.Fatalf("list: %+v %v", listed, err)
	}

	again, err := mediaSvc.UploadForPurpose(ctx, leader, media.PurposeCertificateFont, media.UploadedFile{Name: "brandon2.ttf", Data: rawTTF("again")})
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.CreateFont(ctx, leader, certificate.FontInput{MediaID: again.ID, OwnerTeam: "artlab"})
	var exists *certificate.FontExistsError
	if !errors.As(err, &exists) || exists.FontID != font.ID {
		t.Fatalf("same face: %v", err)
	}

	item, err := service.CreateTemplate(ctx, leader, certificate.TemplateDraft{Name: "ARTLAB", OwnerTeam: "ARTLAB", SourceKind: "sky", Layout: fontLayout(font.MediaID)})
	if err != nil {
		t.Fatalf("template with the font: %v", err)
	}
	var layoutLinks int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM media_attachments
		WHERE media_id = $1 AND owner_type = 'certificate_template' AND role = 'certificate_asset' AND owner_id = $2`, font.MediaID, item.ID).Scan(&layoutLinks); err != nil || layoutLinks != 1 {
		t.Fatalf("layout link: %d %v", layoutLinks, err)
	}
	if using, err := store.TemplatesUsingMedia(ctx, font.MediaID); err != nil || len(using) != 1 || using[0].ID != item.ID {
		t.Fatalf("templates using the font: %+v %v", using, err)
	}

	if err := service.ArchiveFont(ctx, leader, font.ID); err != nil {
		t.Fatal(err)
	}
	if listed, err := service.ListFonts(ctx, leader, "ARTLAB"); err != nil || len(listed) != 0 {
		t.Fatalf("list after archive: %+v %v", listed, err)
	}
	if s, _ := status(font.MediaID); s != media.StatusAttached {
		t.Fatalf("archived font's file: %s", s)
	}
	if _, err := service.PublishTemplate(ctx, leader, item.ID); err != nil {
		t.Fatalf("publish after archive: %v", err)
	}
	if !strings.Contains(render.html, "@font-face{font-family:'cf-"+font.MediaID.String()+"'") {
		t.Fatal("the version does not embed the font")
	}
	// A new face of the same family may come back once the first is
	// archived.
	if _, err := service.CreateFont(ctx, leader, certificate.FontInput{MediaID: again.ID, OwnerTeam: "ARTLAB"}); err != nil {
		t.Fatalf("after archive: %v", err)
	}
}
