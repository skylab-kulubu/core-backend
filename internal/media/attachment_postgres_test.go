package media_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/certificate"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/media"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
	"github.com/skylab-kulubu/core-backend/internal/testpostgres"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// mediaDatabase is a migrated database with one signed-in organizer who may
// upload Event covers.
type mediaDatabase struct {
	pool      *pgxpool.Pool
	store     *media.PostgresStore
	blobs     *media.MemoryBlob
	svc       media.Service
	organizer authz.Principal
}

func newMediaDatabase(t *testing.T) mediaDatabase {
	t.Helper()
	pool := testpostgres.Start(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	organizer := uuid.New()
	if _, _, err := user.NewService(user.NewPostgresStore(pool)).Ensure(ctx, organizer, user.Profile{
		Email: "organizer@example.com", FirstName: "Ada", LastName: "Organizer", Username: "organizer",
	}); err != nil {
		t.Fatal(err)
	}
	store := media.NewPostgresStore(pool)
	blobs := media.NewMemoryBlob()
	return mediaDatabase{
		pool: pool, store: store, blobs: blobs,
		// Skyforms and the CMS both have a service client here, so their
		// purposes can be uploaded and attached.
		svc: media.NewServiceWithOptions(store, blobs, authz.NewAuthorizer(authz.DefaultPolicy()), "",
			media.ServiceOptions{ServiceProducts: authz.ServiceProducts}),
		organizer: authz.Principal{ID: organizer.String(), Groups: []string{"/UYELER/YK"}},
	}
}

func (d mediaDatabase) upload(t *testing.T, purpose string) media.Media {
	t.Helper()
	created, err := d.svc.UploadForPurpose(context.Background(), d.organizer, purpose, uploaded("photo.png", "image/png", pngDot()))
	if err != nil {
		t.Fatal(err)
	}
	return created
}

func (d mediaDatabase) get(t *testing.T, id uuid.UUID) media.Media {
	t.Helper()
	got, err := d.store.GetIncludingDeleted(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestPostgresUploadedMediaIsPendingUntilItsPendingTTL(t *testing.T) {
	db := newMediaDatabase(t)

	before := time.Now()
	cover := db.upload(t, "event_cover")
	after := time.Now()

	got := db.get(t, cover.ID)
	if got.Status != media.StatusPending || got.ExpiresAt == nil ||
		got.ExpiresAt.Before(before.Add(24*time.Hour).Truncate(time.Microsecond)) || got.ExpiresAt.After(after.Add(24*time.Hour)) {
		t.Fatalf("status %q expires %v, want pending until 24h after the upload", got.Status, got.ExpiresAt)
	}
}

// detachedWindow checks a Media detached between before and after: purged
// 30 days later unless something attaches it again.
func detachedWindow(t *testing.T, got media.Media, before, after time.Time) {
	t.Helper()
	window := 30 * 24 * time.Hour
	if got.Status != media.StatusDetached || got.ExpiresAt == nil ||
		got.ExpiresAt.Before(before.Add(window).Truncate(time.Second)) || got.ExpiresAt.After(after.Add(window)) {
		t.Fatalf("media %s status %q expires %v, want detached for 30 days", got.ID, got.Status, got.ExpiresAt)
	}
}

func attached(t *testing.T, got media.Media) {
	t.Helper()
	if got.Status != media.StatusAttached || got.ExpiresAt != nil {
		t.Fatalf("media %s status %q expires %v, want attached with no expiry", got.ID, got.Status, got.ExpiresAt)
	}
}

func TestPostgresEventCoverAttachesItsMediaAndDetachesTheOneItReplaces(t *testing.T) {
	db := newMediaDatabase(t)
	ctx := context.Background()
	events := event.NewService(event.NewPostgresStore(db.pool), authz.NewAuthorizer(authz.DefaultPolicy()))
	first := db.upload(t, "event_cover")
	second := db.upload(t, "event_cover")

	created, err := events.Create(ctx, db.organizer, event.Event{Name: "Hack", Location: "YTÜ", OwnerTeam: "WEBLAB", CoverImageID: &first.ID})
	if err != nil {
		t.Fatal(err)
	}
	attached(t, db.get(t, first.ID))

	before := time.Now()
	created.CoverImageID = &second.ID
	if _, err := events.Update(ctx, db.organizer, created.ID, created); err != nil {
		t.Fatal(err)
	}
	after := time.Now()
	attached(t, db.get(t, second.ID))
	detachedWindow(t, db.get(t, first.ID), before, after)
}

func TestPostgresEventGalleryAttachesAndDetachesItsMedia(t *testing.T) {
	db := newMediaDatabase(t)
	ctx := context.Background()
	events := event.NewService(event.NewPostgresStore(db.pool), authz.NewAuthorizer(authz.DefaultPolicy()))
	photo := db.upload(t, "event_gallery")
	cover := db.upload(t, "event_cover")
	created, err := events.Create(ctx, db.organizer, event.Event{Name: "Hack", Location: "YTÜ", OwnerTeam: "WEBLAB", CoverImageID: &cover.ID})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := events.AddImages(ctx, db.organizer, created.ID, []uuid.UUID{photo.ID, cover.ID}); err != nil {
		t.Fatal(err)
	}
	attached(t, db.get(t, photo.ID))

	before := time.Now()
	if _, err := events.RemoveImages(ctx, db.organizer, created.ID, []uuid.UUID{photo.ID, cover.ID}); err != nil {
		t.Fatal(err)
	}
	after := time.Now()
	detachedWindow(t, db.get(t, photo.ID), before, after)
	// Still the Event's cover: one attachment is left.
	attached(t, db.get(t, cover.ID))
}

func TestPostgresProfilePictureReplacementDetachesThePreviousPicture(t *testing.T) {
	db := newMediaDatabase(t)
	ctx := context.Background()
	users := user.NewService(user.NewPostgresStore(db.pool))
	person := uuid.MustParse(db.organizer.ID)
	first := db.upload(t, "profile_picture")
	second := db.upload(t, "profile_picture")

	if _, err := users.SetProfilePicture(ctx, person, first.ID, first.Key); err != nil {
		t.Fatal(err)
	}
	attached(t, db.get(t, first.ID))

	before := time.Now()
	if _, err := users.SetProfilePicture(ctx, person, second.ID, second.Key); err != nil {
		t.Fatal(err)
	}
	after := time.Now()
	attached(t, db.get(t, second.ID))
	detachedWindow(t, db.get(t, first.ID), before, after)

	before = time.Now()
	if _, err := users.ClearProfilePicture(ctx, person); err != nil {
		t.Fatal(err)
	}
	after = time.Now()
	detachedWindow(t, db.get(t, second.ID), before, after)
}

func TestPostgresCertificateTemplateAssetsAttachTheirMedia(t *testing.T) {
	db := newMediaDatabase(t)
	ctx := context.Background()
	templates := certificate.NewPostgresStore(db.pool)
	legacy := func() media.Media {
		t.Helper()
		created, err := db.svc.Upload(ctx, db.organizer, "asset.png", "image/png", pngDot())
		if err != nil {
			t.Fatal(err)
		}
		return created
	}
	background, logo, published := legacy(), legacy(), legacy()
	layout := func(background, element uuid.UUID) certificate.Layout {
		return certificate.Layout{
			Width: 297, Height: 210, Orientation: "landscape", BackgroundMediaID: &background,
			Elements: []certificate.Element{{ID: "logo", Kind: "image", MediaID: &element, Width: 10, Height: 10}},
		}
	}

	template, err := templates.CreateTemplate(ctx, certificate.Template{
		ID: uuid.New(), Name: "Draft", OwnerTeam: "WEBLAB", SourceKind: "upload", DraftLayout: layout(background.ID, logo.ID),
	})
	if err != nil {
		t.Fatal(err)
	}
	attached(t, db.get(t, background.ID))
	attached(t, db.get(t, logo.ID))

	if _, err := templates.CreateVersion(ctx, certificate.TemplateVersion{
		ID: uuid.New(), TemplateID: template.ID, Layout: layout(background.ID, logo.ID), Checksum: "v1",
		AssetManifest: map[string]certificate.VersionAssetRef{published.ID.String(): {Key: "certificate-template-assets/x", ContentType: "image/png"}},
	}); err != nil {
		t.Fatal(err)
	}
	attached(t, db.get(t, published.ID))

	// The draft drops the logo; the published version still uses it.
	template.DraftLayout = layout(background.ID, background.ID)
	if _, err := templates.UpdateTemplate(ctx, template); err != nil {
		t.Fatal(err)
	}
	attached(t, db.get(t, logo.ID))

	// A draft-only asset is detached when the draft drops it; being legacy,
	// it gets no expiry.
	extra := legacy()
	template.DraftLayout = layout(background.ID, extra.ID)
	if _, err := templates.UpdateTemplate(ctx, template); err != nil {
		t.Fatal(err)
	}
	attached(t, db.get(t, extra.ID))
	template.DraftLayout = layout(background.ID, background.ID)
	if _, err := templates.UpdateTemplate(ctx, template); err != nil {
		t.Fatal(err)
	}
	if got := db.get(t, extra.ID); got.Status != media.StatusDetached || got.ExpiresAt != nil {
		t.Fatalf("dropped asset: status %q expires %v, want detached with no expiry", got.Status, got.ExpiresAt)
	}
}

func TestPostgresMediaAttachmentFollowsItsLinksTransaction(t *testing.T) {
	db := newMediaDatabase(t)
	ctx := context.Background()
	events := event.NewService(event.NewPostgresStore(db.pool), authz.NewAuthorizer(authz.DefaultPolicy()))
	first := db.upload(t, "event_cover")
	second := db.upload(t, "event_cover")
	created, err := events.Create(ctx, db.organizer, event.Event{Name: "Hack", Location: "YTÜ", OwnerTeam: "WEBLAB", CoverImageID: &first.ID})
	if err != nil {
		t.Fatal(err)
	}

	// The cover changes, then the same transaction fails on a door staff
	// member who is nobody: neither Media moves.
	created.CoverImageID = &second.ID
	created.DoorStaffIDs = []uuid.UUID{uuid.New()}
	if _, err := events.Update(ctx, db.organizer, created.ID, created); err == nil {
		t.Fatal("update with an unknown door staff member succeeded")
	}
	attached(t, db.get(t, first.ID))
	if got := db.get(t, second.ID); got.Status != media.StatusPending || got.ExpiresAt == nil {
		t.Fatalf("media of a rolled back link: status %q expires %v", got.Status, got.ExpiresAt)
	}
}

func TestPostgresMediaAttachmentRefusesInactiveMedia(t *testing.T) {
	db := newMediaDatabase(t)
	ctx := context.Background()
	archived := db.upload(t, "event_gallery")
	if err := db.store.Archive(ctx, archived.ID, nil); err != nil {
		t.Fatal(err)
	}
	purged := db.upload(t, "event_gallery")
	if _, err := db.pool.Exec(ctx, `UPDATE media SET blob_purge_started_at = now() WHERE id = $1`, purged.ID); err != nil {
		t.Fatal(err)
	}
	for name, id := range map[string]uuid.UUID{"archived": archived.ID, "purge started": purged.ID} {
		if _, err := db.pool.Exec(ctx, `INSERT INTO media_attachments (media_id, owner_service, owner_type, owner_id, role)
			VALUES ($1, 'cms', 'page', $2, 'cms_image')`, id, uuid.New()); err == nil {
			t.Errorf("attachment to %s media accepted", name)
		}
	}
}

// A legacy Media may still be used outside core, by its address (CMS content
// stores addresses): detaching it from its last core record sets no expiry.
// Only the legacy backfill reports and removes unused legacy Media.
func TestPostgresDetachedLegacyMediaKeepsNoExpiry(t *testing.T) {
	db := newMediaDatabase(t)
	ctx := context.Background()
	events := event.NewService(event.NewPostgresStore(db.pool), authz.NewAuthorizer(authz.DefaultPolicy()))
	legacy, err := db.svc.Upload(ctx, db.organizer, "poster.png", "image/png", pngDot())
	if err != nil {
		t.Fatal(err)
	}
	purposed := db.upload(t, "event_cover")
	created, err := events.Create(ctx, db.organizer, event.Event{Name: "Hack", Location: "YTÜ", OwnerTeam: "WEBLAB", CoverImageID: &legacy.ID})
	if err != nil {
		t.Fatal(err)
	}
	attached(t, db.get(t, legacy.ID))

	created.CoverImageID = &purposed.ID
	if _, err := events.Update(ctx, db.organizer, created.ID, created); err != nil {
		t.Fatal(err)
	}
	if got := db.get(t, legacy.ID); got.Status != media.StatusDetached || got.ExpiresAt != nil {
		t.Fatalf("detached legacy Media: status %q expires %v, want detached with no expiry", got.Status, got.ExpiresAt)
	}
}
