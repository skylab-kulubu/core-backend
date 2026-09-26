package media_test

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/certificate"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/media"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// storedBeforePurposes is a Media as the media redesign found it: legacy,
// pending, no expiry.
func (d mediaDatabase) storedBeforePurposes(t *testing.T, name string) media.Media {
	t.Helper()
	created, err := d.svc.Upload(context.Background(), d.organizer, name, "image/png", pngDot())
	if err != nil {
		t.Fatal(err)
	}
	if created.Purpose != media.PurposeLegacy || created.ExpiresAt != nil {
		t.Fatalf("%s stored as %q expiring %v, want legacy with no expiry", name, created.Purpose, created.ExpiresAt)
	}
	return created
}

func (d mediaDatabase) events() event.Service {
	return event.NewService(event.NewPostgresStore(d.pool), authz.NewAuthorizer(authz.DefaultPolicy()))
}

func (d mediaDatabase) backfillPurposes(t *testing.T) media.LegacyPurposeReport {
	t.Helper()
	catalogue, err := media.LoadCatalogue()
	if err != nil {
		t.Fatal(err)
	}
	report, err := media.BackfillLegacyPurposes(context.Background(), d.store, catalogue, func(err error) {
		t.Errorf("backfill failure: %v", err)
	})
	if err != nil {
		t.Fatal(err)
	}
	return report
}

// purposeIs checks a Media's purpose and that the backfill changed nothing
// else on it: not its status, expiry, address or type.
func (d mediaDatabase) purposeIs(t *testing.T, before media.Media, want string) {
	t.Helper()
	got := d.get(t, before.ID)
	if got.Purpose != want {
		t.Fatalf("media %s purpose %q, want %q", before.Name, got.Purpose, want)
	}
	got.Purpose, before.Purpose = "", ""
	if got.Status != before.Status || (got.ExpiresAt == nil) != (before.ExpiresAt == nil) ||
		got.Key != before.Key || got.Type != before.Type || got.Size != before.Size || !got.UpdatedAt.Equal(before.UpdatedAt) {
		t.Fatalf("media %s changed beyond its purpose:\n got %+v\nwant %+v", before.Name, got, before)
	}
}

func TestPostgresLegacyBackfillGivesEachCoreUseItsPurpose(t *testing.T) {
	db := newMediaDatabase(t)
	ctx := context.Background()
	cover := db.storedBeforePurposes(t, "cover.png")
	photo := db.storedBeforePurposes(t, "photo.png")
	picture := db.storedBeforePurposes(t, "me.png")
	created, err := db.events().Create(ctx, db.organizer, event.Event{Name: "Hack", Location: "YTÜ", OwnerTeam: "WEBLAB", CoverImageID: &cover.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.events().AddImages(ctx, db.organizer, created.ID, []uuid.UUID{photo.ID}); err != nil {
		t.Fatal(err)
	}
	if _, err := user.NewService(user.NewPostgresStore(db.pool)).SetProfilePicture(ctx, uuid.MustParse(db.organizer.ID), picture.ID, picture.Key); err != nil {
		t.Fatal(err)
	}
	cover, photo, picture = db.get(t, cover.ID), db.get(t, photo.ID), db.get(t, picture.ID)

	report := db.backfillPurposes(t)

	if report.Assigned != 3 || report.Failed != 0 {
		t.Fatalf("report %+v", report)
	}
	db.purposeIs(t, cover, media.PurposeEventCover)
	db.purposeIs(t, photo, media.PurposeEventGallery)
	db.purposeIs(t, picture, media.PurposeProfilePicture)
}

// certificate_asset is a private purpose: giving it to a Media whose blob is
// public would claim an encryption that never happened. Certificate template
// assets stay legacy for good (decision G1).
func TestPostgresLegacyBackfillKeepsCertificateTemplateAssetsLegacy(t *testing.T) {
	db := newMediaDatabase(t)
	ctx := context.Background()
	templates := certificate.NewPostgresStore(db.pool)
	background := db.storedBeforePurposes(t, "background.png")
	published := db.storedBeforePurposes(t, "logo.png")
	layout := certificate.Layout{Width: 297, Height: 210, Orientation: "landscape", BackgroundMediaID: &background.ID, Elements: []certificate.Element{}}
	template, err := templates.CreateTemplate(ctx, certificate.Template{
		ID: uuid.New(), Name: "Katılım", OwnerTeam: "WEBLAB", SourceKind: "upload", DraftLayout: layout,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := templates.CreateVersion(ctx, certificate.TemplateVersion{
		ID: uuid.New(), TemplateID: template.ID, Layout: layout, Checksum: "v1",
		AssetManifest: map[string]certificate.VersionAssetRef{published.ID.String(): {Key: "certificate-template-assets/x", ContentType: "image/png"}},
	}); err != nil {
		t.Fatal(err)
	}
	background, published = db.get(t, background.ID), db.get(t, published.ID)

	report := db.backfillPurposes(t)

	if report != (media.LegacyPurposeReport{KeptPrivate: 2}) {
		t.Fatalf("report %+v, want the two assets kept legacy as private", report)
	}
	db.purposeIs(t, background, media.PurposeLegacy)
	db.purposeIs(t, published, media.PurposeLegacy)
	attached(t, db.get(t, background.ID))
}

// A Media gets a purpose only when that one purpose fits every Media
// attachment it has, as a new link would be checked. Both Event purposes fit
// both Event roles, so a Media that is a cover and a gallery photo takes the
// cover's (roles are tried cover, gallery, profile picture). Uses that no
// one purpose fits, such as an Event cover that is also someone's profile
// picture, a certificate asset or a CMS page's image, keep it legacy.
func TestPostgresLegacyBackfillPicksOnePurposeThatFitsEveryUse(t *testing.T) {
	db := newMediaDatabase(t)
	ctx := context.Background()
	coverAndPhoto := db.storedBeforePurposes(t, "cover-and-photo.png")
	photoTwice := db.storedBeforePurposes(t, "photo-twice.png")
	coverAndPicture := db.storedBeforePurposes(t, "cover-and-me.png")
	coverAndCertificate := db.storedBeforePurposes(t, "cover-and-certificate.png")
	coverAndPage := db.storedBeforePurposes(t, "cover-and-page.png")

	first, err := db.events().Create(ctx, db.organizer, event.Event{Name: "Hack", Location: "YTÜ", OwnerTeam: "WEBLAB", CoverImageID: &coverAndPhoto.ID})
	if err != nil {
		t.Fatal(err)
	}
	second, err := db.events().Create(ctx, db.organizer, event.Event{Name: "Jam", Location: "YTÜ", OwnerTeam: "WEBLAB", CoverImageID: &coverAndPicture.ID})
	if err != nil {
		t.Fatal(err)
	}
	third, err := db.events().Create(ctx, db.organizer, event.Event{Name: "Talk", Location: "YTÜ", OwnerTeam: "WEBLAB", CoverImageID: &coverAndCertificate.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.events().Create(ctx, db.organizer, event.Event{Name: "Meetup", Location: "YTÜ", OwnerTeam: "WEBLAB", CoverImageID: &coverAndPage.ID}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []uuid.UUID{first.ID, second.ID} {
		if _, err := db.events().AddImages(ctx, db.organizer, id, []uuid.UUID{photoTwice.ID}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.events().AddImages(ctx, db.organizer, third.ID, []uuid.UUID{coverAndPhoto.ID}); err != nil {
		t.Fatal(err)
	}
	if _, err := user.NewService(user.NewPostgresStore(db.pool)).SetProfilePicture(ctx, uuid.MustParse(db.organizer.ID), coverAndPicture.ID, coverAndPicture.Key); err != nil {
		t.Fatal(err)
	}
	if _, err := certificate.NewPostgresStore(db.pool).CreateTemplate(ctx, certificate.Template{
		ID: uuid.New(), Name: "Katılım", OwnerTeam: "WEBLAB", SourceKind: "upload",
		DraftLayout: certificate.Layout{Width: 297, Height: 210, Orientation: "landscape", BackgroundMediaID: &coverAndCertificate.ID, Elements: []certificate.Element{}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.pool.Exec(ctx, `INSERT INTO media_attachments (media_id, owner_service, owner_type, owner_id, role)
		VALUES ($1, 'cms', 'page', 'skylab-site:hakkimizda', 'image')`, coverAndPage.ID); err != nil {
		t.Fatal(err)
	}
	before := map[uuid.UUID]media.Media{}
	for _, item := range []media.Media{coverAndPhoto, photoTwice, coverAndPicture, coverAndCertificate, coverAndPage} {
		before[item.ID] = db.get(t, item.ID)
	}

	report := db.backfillPurposes(t)

	if report != (media.LegacyPurposeReport{Assigned: 2, KeptMixed: 3}) {
		t.Fatalf("report %+v", report)
	}
	db.purposeIs(t, before[coverAndPhoto.ID], media.PurposeEventCover)
	db.purposeIs(t, before[photoTwice.ID], media.PurposeEventGallery)
	db.purposeIs(t, before[coverAndPicture.ID], media.PurposeLegacy)
	db.purposeIs(t, before[coverAndCertificate.ID], media.PurposeLegacy)
	db.purposeIs(t, before[coverAndPage.ID], media.PurposeLegacy)
}

// Only legacy Media core attaches change. A legacy Media nothing attaches
// (an orphan: maybe a Skyforms answer or a CMS image, used by address where
// core cannot see) keeps its purpose, status and missing expiry; so does a
// Media that already has a purpose. A second pass changes nothing.
func TestPostgresLegacyBackfillLeavesOrphansAndPurposedMediaAndCanRunAgain(t *testing.T) {
	db := newMediaDatabase(t)
	ctx := context.Background()
	never := db.storedBeforePurposes(t, "answer.png")
	dropped := db.storedBeforePurposes(t, "old-cover.png")
	cover := db.storedBeforePurposes(t, "cover.png")
	purposed := db.upload(t, "event_gallery")
	created, err := db.events().Create(ctx, db.organizer, event.Event{Name: "Hack", Location: "YTÜ", OwnerTeam: "WEBLAB", CoverImageID: &dropped.ID})
	if err != nil {
		t.Fatal(err)
	}
	created.CoverImageID = &purposed.ID
	if _, err := db.events().Update(ctx, db.organizer, created.ID, created); err != nil {
		t.Fatal(err)
	}
	if _, err := db.events().Create(ctx, db.organizer, event.Event{Name: "Jam", Location: "YTÜ", OwnerTeam: "WEBLAB", CoverImageID: &cover.ID}); err != nil {
		t.Fatal(err)
	}
	before := map[uuid.UUID]media.Media{}
	for _, item := range []media.Media{never, dropped, cover, purposed} {
		before[item.ID] = db.get(t, item.ID)
	}

	if report := db.backfillPurposes(t); report != (media.LegacyPurposeReport{Assigned: 1}) {
		t.Fatalf("first pass %+v", report)
	}
	db.purposeIs(t, before[never.ID], media.PurposeLegacy)
	db.purposeIs(t, before[dropped.ID], media.PurposeLegacy)
	db.purposeIs(t, before[purposed.ID], "event_gallery")
	db.purposeIs(t, before[cover.ID], media.PurposeEventCover)
	for _, orphan := range []media.Media{never, dropped} {
		if got := db.get(t, orphan.ID); got.ExpiresAt != nil {
			t.Fatalf("orphan %s expires %v", orphan.Name, got.ExpiresAt)
		}
	}

	if report := db.backfillPurposes(t); report != (media.LegacyPurposeReport{}) {
		t.Fatalf("second pass %+v", report)
	}
	db.purposeIs(t, before[cover.ID], media.PurposeEventCover)
}

// A Media the backfill cannot update is reported by id and skipped; it never
// holds up the ones after it, and the next pass finishes it.
func TestPostgresLegacyBackfillWalksPastAMediaItCannotUpdate(t *testing.T) {
	db := newMediaDatabase(t)
	ctx := context.Background()
	// More than one batch of gallery photos.
	photos := make([]uuid.UUID, 0, 30)
	for range 30 {
		photos = append(photos, db.storedBeforePurposes(t, "photo.png").ID)
	}
	created, err := db.events().Create(ctx, db.organizer, event.Event{Name: "Hack", Location: "YTÜ", OwnerTeam: "WEBLAB"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.events().AddImages(ctx, db.organizer, created.ID, photos); err != nil {
		t.Fatal(err)
	}
	stuck := photos[3]
	if _, err := db.pool.Exec(ctx, `
		CREATE FUNCTION refuse_stuck_media() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN RAISE EXCEPTION 'disk full'; END; $$;
		CREATE TRIGGER stuck_media BEFORE UPDATE ON media FOR EACH ROW
		WHEN (OLD.id = '`+stuck.String()+`') EXECUTE FUNCTION refuse_stuck_media();`); err != nil {
		t.Fatal(err)
	}
	catalogue, err := media.LoadCatalogue()
	if err != nil {
		t.Fatal(err)
	}

	var failures []error
	report, err := media.BackfillLegacyPurposes(ctx, db.store, catalogue, func(err error) { failures = append(failures, err) })
	if err != nil {
		t.Fatal(err)
	}
	if report != (media.LegacyPurposeReport{Assigned: 29, Failed: 1}) || len(failures) != 1 || !strings.Contains(failures[0].Error(), stuck.String()) {
		t.Fatalf("report %+v failures %v", report, failures)
	}
	for _, id := range photos {
		if want := map[bool]string{true: media.PurposeLegacy, false: media.PurposeEventGallery}[id == stuck]; db.get(t, id).Purpose != want {
			t.Fatalf("media %s purpose %q, want %q", id, db.get(t, id).Purpose, want)
		}
	}

	if _, err := db.pool.Exec(ctx, `DROP TRIGGER stuck_media ON media`); err != nil {
		t.Fatal(err)
	}
	if report := db.backfillPurposes(t); report != (media.LegacyPurposeReport{Assigned: 1}) {
		t.Fatalf("retry %+v", report)
	}
	if got := db.get(t, stuck).Purpose; got != media.PurposeEventGallery {
		t.Fatalf("retried media purpose %q", got)
	}
}
