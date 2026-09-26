package media_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/media"
)

// withBlob uploads a Media for purpose and keeps its blob to watch.
func (d mediaDatabase) withBlob(t *testing.T, purpose string) media.Media {
	t.Helper()
	if purpose == media.PurposeLegacy {
		created, err := d.svc.Upload(context.Background(), d.organizer, "legacy.png", "image/png", pngDot())
		if err != nil {
			t.Fatal(err)
		}
		return created
	}
	return d.upload(t, purpose)
}

func (d mediaDatabase) purged(t *testing.T, item media.Media) bool {
	t.Helper()
	got := d.get(t, item.ID)
	_, blobKept := d.blobs.Get(item.Key)
	if (got.BlobPurgedAt != nil) == blobKept {
		t.Fatalf("media %s: purged at %v but blob kept = %v", item.ID, got.BlobPurgedAt, blobKept)
	}
	return got.BlobPurgedAt != nil
}

func TestPostgresCleanupPurgesPendingAndDetachedMediaOnlyAfterTheirExpiry(t *testing.T) {
	db := newMediaDatabase(t)
	ctx := context.Background()
	events := event.NewService(event.NewPostgresStore(db.pool), authz.NewAuthorizer(authz.DefaultPolicy()))
	abandoned := db.withBlob(t, "event_cover")
	legacy := db.withBlob(t, media.PurposeLegacy)
	cover := db.withBlob(t, "event_cover")
	replaced := db.withBlob(t, "event_cover")
	archived := db.withBlob(t, "event_cover")
	if err := db.store.Archive(ctx, archived.ID, nil); err != nil {
		t.Fatal(err)
	}
	created, err := events.Create(ctx, db.organizer, event.Event{Name: "Hack", Location: "YTÜ", OwnerTeam: "WEBLAB", CoverImageID: &replaced.ID})
	if err != nil {
		t.Fatal(err)
	}
	created.CoverImageID = &cover.ID
	if _, err := events.Update(ctx, db.organizer, created.ID, created); err != nil {
		t.Fatal(err)
	}
	now := time.Now()

	// Before the pending TTL nothing goes.
	report, err := media.PurgeExpired(ctx, db.store, db.blobs, now.Add(23*time.Hour), nil)
	if err != nil || report.Purged != 0 {
		t.Fatalf("report %+v err %v", report, err)
	}
	for _, item := range []media.Media{abandoned, legacy, cover, replaced, archived} {
		if db.purged(t, item) {
			t.Fatalf("media %s purged before its expiry", item.ID)
		}
	}

	// After 24 hours the Media nothing attached is purged, and archived as
	// its blob goes.
	report, err = media.PurgeExpired(ctx, db.store, db.blobs, now.Add(25*time.Hour), nil)
	if err != nil || report.Purged != 1 || report.Failed != 0 {
		t.Fatalf("report %+v err %v", report, err)
	}
	if !db.purged(t, abandoned) || db.get(t, abandoned.ID).DeletedAt == nil {
		t.Fatalf("abandoned upload after 24 hours: %+v", db.get(t, abandoned.ID))
	}
	if _, err := db.svc.Get(ctx, abandoned.ID); !errors.Is(err, media.ErrNotFound) {
		t.Fatalf("purged Media still current: %v", err)
	}

	// After 30 days the replaced cover goes too. The attached cover, the
	// legacy upload with no expiry and the archived Media (the archive
	// window's business) stay.
	report, err = media.PurgeExpired(ctx, db.store, db.blobs, now.Add(31*24*time.Hour), nil)
	if err != nil || report.Purged != 1 || report.Failed != 0 {
		t.Fatalf("report %+v err %v", report, err)
	}
	if !db.purged(t, replaced) {
		t.Fatal("detached cover kept after 30 days")
	}
	for name, item := range map[string]media.Media{"attached": cover, "legacy": legacy, "archived": archived} {
		if db.purged(t, item) {
			t.Errorf("%s Media purged by the expiry cleanup", name)
		}
	}

	// A second pass finds nothing left to do.
	report, err = media.PurgeExpired(ctx, db.store, db.blobs, now.Add(31*24*time.Hour), nil)
	if err != nil || report != (media.ExpiryReport{}) {
		t.Fatalf("second pass %+v err %v", report, err)
	}
}

// failingKeyBlob fails to delete one object.
type failingKeyBlob struct {
	*media.MemoryBlob
	key string
}

func (b failingKeyBlob) Delete(ctx context.Context, key string) error {
	if key == b.key {
		return errors.New("r2 unavailable")
	}
	return b.MemoryBlob.Delete(ctx, key)
}

func TestPostgresCleanupWalksPastAMediaItCannotPurge(t *testing.T) {
	db := newMediaDatabase(t)
	ctx := context.Background()
	// More than one batch of abandoned uploads.
	uploads := make([]media.Media, 0, 30)
	for range 30 {
		uploads = append(uploads, db.withBlob(t, "event_gallery"))
	}
	stuck := uploads[3]
	later := time.Now().Add(25 * time.Hour)

	var failures []error
	report, err := media.PurgeExpired(ctx, db.store, failingKeyBlob{MemoryBlob: db.blobs, key: stuck.Key}, later, func(err error) {
		failures = append(failures, err)
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.Purged != 29 || report.Failed != 1 || len(failures) != 1 {
		t.Fatalf("report %+v failures %v", report, failures)
	}
	if want := stuck.ID.String(); !strings.Contains(failures[0].Error(), want) {
		t.Fatalf("failure %q does not name media %s", failures[0], want)
	}
	for _, item := range uploads {
		if item.ID != stuck.ID && !db.purged(t, item) {
			t.Fatalf("media %s kept behind the one that failed", item.ID)
		}
	}
	if db.purged(t, stuck) {
		t.Fatal("the Media whose blob could not be deleted is marked purged")
	}

	// The next pass finishes it.
	report, err = media.PurgeExpired(ctx, db.store, db.blobs, later, nil)
	if err != nil || report.Purged != 1 || report.Failed != 0 {
		t.Fatalf("retry report %+v err %v", report, err)
	}
	if !db.purged(t, stuck) {
		t.Fatal("retry left the blob")
	}
}

func TestPostgresPurgeKeepsMediaAnAttachmentOrACoreLinkStillUses(t *testing.T) {
	db := newMediaDatabase(t)
	ctx := context.Background()

	// A CMS page's Media attachment keeps an archived Media past the
	// archive window: the reference check is "has any attachment".
	onPage := db.withBlob(t, "event_gallery")
	if _, err := db.pool.Exec(ctx, `INSERT INTO media_attachments (media_id, owner_service, owner_type, owner_id, role)
		VALUES ($1, 'cms', 'page', $2, 'cms_image')`, onPage.ID, uuid.New()); err != nil {
		t.Fatal(err)
	}
	if err := db.store.Archive(ctx, onPage.ID, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := db.pool.Exec(ctx, `UPDATE media SET deleted_at = now() - interval '31 days' WHERE id = $1`, onPage.ID); err != nil {
		t.Fatal(err)
	}
	report, err := media.PurgeDeleted(ctx, db.store, db.blobs, time.Now(), media.DefaultBlobRecoveryWindow, 25)
	if err != nil || report.Purged != 0 || report.Referenced != 1 {
		t.Fatalf("archive report %+v err %v", report, err)
	}
	if db.purged(t, onPage) {
		t.Fatal("archived Media with a Media attachment was purged")
	}

	// Safety net: an Event cover linked without its Media attachment still
	// keeps an expired upload.
	cover := db.withBlob(t, "event_cover")
	if _, err := db.pool.Exec(ctx, `ALTER TABLE events DISABLE TRIGGER events_media_attachments_insert`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.pool.Exec(ctx, `INSERT INTO events (id, name, location, owner_team, cover_image_id) VALUES ($1, 'Unattached', 'YTÜ', 'WEBLAB', $2)`, uuid.New(), cover.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.pool.Exec(ctx, `ALTER TABLE events ENABLE TRIGGER events_media_attachments_insert`); err != nil {
		t.Fatal(err)
	}
	expiry, err := media.PurgeExpired(ctx, db.store, db.blobs, time.Now().Add(25*time.Hour), nil)
	if err != nil || expiry.Purged != 0 || expiry.Kept != 1 {
		t.Fatalf("expiry report %+v err %v", expiry, err)
	}
	if db.purged(t, cover) {
		t.Fatal("expired Media still used as an Event cover was purged")
	}
}

// A restored Media no Media attachment keeps starts its purpose's window
// again: an expiry that passed while it was archived would purge it on the
// next pass.
func TestPostgresRestoreStartsTheExpiryAgain(t *testing.T) {
	db := newMediaDatabase(t)
	ctx := context.Background()
	cover := db.withBlob(t, "event_cover")
	legacy := db.withBlob(t, media.PurposeLegacy)
	for _, item := range []media.Media{cover, legacy} {
		if err := db.svc.Delete(ctx, db.organizer, item.ID); err != nil {
			t.Fatal(err)
		}
	}
	// Archived for two days: the upload's 24 hours are long gone.
	if _, err := db.pool.Exec(ctx, `UPDATE media SET expires_at = now() - interval '1 day' WHERE id = $1`, cover.ID); err != nil {
		t.Fatal(err)
	}

	before := time.Now()
	for _, item := range []media.Media{cover, legacy} {
		if _, err := db.svc.Restore(ctx, db.organizer, item.ID); err != nil {
			t.Fatal(err)
		}
	}
	after := time.Now()
	if got := db.get(t, cover.ID); got.ExpiresAt == nil ||
		got.ExpiresAt.Before(before.Add(24*time.Hour).Truncate(time.Microsecond)) || got.ExpiresAt.After(after.Add(24*time.Hour)) {
		t.Fatalf("restored cover expires %v, want 24h after the restore", got.ExpiresAt)
	}
	if got := db.get(t, legacy.ID); got.ExpiresAt != nil {
		t.Fatalf("restored legacy Media expires %v, want no expiry", got.ExpiresAt)
	}
	report, err := media.PurgeExpired(ctx, db.store, db.blobs, time.Now().Add(time.Hour), nil)
	if err != nil || report.Purged != 0 {
		t.Fatalf("report %+v err %v", report, err)
	}
}

// A Media with no expiry is simply not expired: asking to purge it keeps it.
func TestPostgresExpiryPurgeKeepsAMediaWithNoExpiry(t *testing.T) {
	db := newMediaDatabase(t)
	legacy := db.withBlob(t, media.PurposeLegacy)

	ctx := context.Background()
	purged, err := db.store.PurgeExpiredBlobIfUnattached(ctx, legacy.ID, time.Now().Add(365*24*time.Hour), func(key string) error {
		return db.blobs.Delete(ctx, key)
	})
	if err != nil || purged {
		t.Fatalf("purged %v err %v, want kept", purged, err)
	}
	if db.purged(t, legacy) {
		t.Fatal("legacy Media purged")
	}
}
