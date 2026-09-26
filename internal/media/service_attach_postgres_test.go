package media_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/media"
)

func (d mediaDatabase) attachFor(t *testing.T, p authz.Principal, m media.Media, owner media.Owner, role media.Role) media.Attachment {
	t.Helper()
	a, _, err := d.svc.Attach(context.Background(), p, m.ID, owner, role)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// pending → attached → detached through another product's Media
// attachments: the database's status trigger follows them as it follows
// core's own links.
func TestPostgresServiceAttachmentKeepsTheMediaAttachedUntilItsLastGoes(t *testing.T) {
	db := newMediaDatabase(t)
	ctx := context.Background()
	logo := db.upload(t, "cms_image")
	home := media.Owner{Service: "cms", Type: "page", ID: uuid.New()}
	about := media.Owner{Service: "cms", Type: "page", ID: uuid.New()}

	first, created, err := db.svc.Attach(ctx, cmsService, logo.ID, home, media.RoleImage)
	if err != nil || !created {
		t.Fatalf("attach: created %v, err %v", created, err)
	}
	attached(t, db.get(t, logo.ID))
	again, created, err := db.svc.Attach(ctx, cmsService, logo.ID, home, media.RoleImage)
	if err != nil || created || again.ID != first.ID {
		t.Fatalf("same link again: %+v created %v err %v; want %s", again, created, err, first.ID)
	}
	second := db.attachFor(t, cmsService, logo, about, media.RoleImage)

	if err := db.svc.Detach(ctx, cmsService, logo.ID, first.ID); err != nil {
		t.Fatal(err)
	}
	attached(t, db.get(t, logo.ID))
	before := time.Now()
	if err := db.svc.Detach(ctx, cmsService, logo.ID, second.ID); err != nil {
		t.Fatal(err)
	}
	after := time.Now()
	detachedWindow(t, db.get(t, logo.ID), before, after)
	if err := db.svc.Detach(ctx, cmsService, logo.ID, second.ID); err != nil {
		t.Fatalf("detach again: %v", err)
	}

	// Within its window the Media can be attached again.
	db.attachFor(t, cmsService, logo, home, media.RoleImage)
	attached(t, db.get(t, logo.ID))
}

func TestPostgresServiceAttachmentOfALegacyMediaSetsNoExpiry(t *testing.T) {
	db := newMediaDatabase(t)
	ctx := context.Background()
	legacy, err := db.svc.Upload(ctx, db.organizer, "cv.pdf", "application/pdf", []byte("%PDF-1.7\n"))
	if err != nil {
		t.Fatal(err)
	}
	answer := db.attachFor(t, formsService, legacy, media.Owner{Service: "forms", Type: "response", ID: uuid.New()}, media.RoleAnswer)
	attached(t, db.get(t, legacy.ID))

	if err := db.svc.Detach(ctx, formsService, legacy.ID, answer.ID); err != nil {
		t.Fatal(err)
	}
	if got := db.get(t, legacy.ID); got.Status != media.StatusDetached || got.ExpiresAt != nil {
		t.Fatalf("detached legacy Media: status %q expires %v, want detached with no expiry", got.Status, got.ExpiresAt)
	}
}

// The database refuses a Media attachment to a Media that is gone, archived
// or being purged, whatever the service read before: the store answers
// ErrNotLinkable.
func TestPostgresStoreRefusesAnAttachmentToMediaThatIsNotCurrent(t *testing.T) {
	db := newMediaDatabase(t)
	ctx := context.Background()
	archived := db.upload(t, "cms_image")
	if err := db.store.Archive(ctx, archived.ID, nil); err != nil {
		t.Fatal(err)
	}
	purging := db.upload(t, "cms_image")
	if _, err := db.pool.Exec(ctx, `UPDATE media SET blob_purge_started_at = now() WHERE id = $1`, purging.ID); err != nil {
		t.Fatal(err)
	}

	for name, id := range map[string]uuid.UUID{"missing": uuid.New(), "archived": archived.ID, "purge started": purging.ID} {
		_, _, err := db.store.Attach(ctx, media.Attachment{
			MediaID: id, Owner: media.Owner{Service: "cms", Type: "page", ID: uuid.New()}, Role: media.RoleImage,
		})
		if !errors.Is(err, media.ErrNotLinkable) {
			t.Errorf("%s: err = %v, want %v", name, err, media.ErrNotLinkable)
		}
	}
}

// Two calls attaching the same link at once make one Media attachment; both
// answer it.
func TestPostgresConcurrentSameLinkMakesOneAttachment(t *testing.T) {
	db := newMediaDatabase(t)
	logo := db.upload(t, "cms_image")
	owner := media.Owner{Service: "cms", Type: "page", ID: uuid.New()}

	var wg sync.WaitGroup
	results := make([]media.Attachment, 8)
	createdCount := make([]bool, len(results))
	errs := make([]error, len(results))
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], createdCount[i], errs[i] = db.svc.Attach(context.Background(), cmsService, logo.ID, owner, media.RoleImage)
		}()
	}
	wg.Wait()
	created := 0
	for i, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
		if results[i].ID != results[0].ID {
			t.Fatalf("attachments %s and %s for one link", results[i].ID, results[0].ID)
		}
		if createdCount[i] {
			created++
		}
	}
	if created != 1 {
		t.Fatalf("%d calls created the link, want 1", created)
	}
}

// A product cannot remove core's own Media attachments, such as an Event
// cover's.
func TestPostgresProductCannotDetachCoresOwnLinks(t *testing.T) {
	db := newMediaDatabase(t)
	ctx := context.Background()
	events := event.NewService(event.NewPostgresStore(db.pool), authz.NewAuthorizer(authz.DefaultPolicy()))
	cover, err := db.svc.Upload(ctx, db.organizer, "cover.png", "image/png", pngDot())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := events.Create(ctx, db.organizer, event.Event{Name: "Hack", Location: "YTÜ", OwnerTeam: "WEBLAB", CoverImageID: &cover.ID}); err != nil {
		t.Fatal(err)
	}
	var coreAttachment uuid.UUID
	if err := db.pool.QueryRow(ctx, `SELECT id FROM media_attachments WHERE media_id = $1`, cover.ID).Scan(&coreAttachment); err != nil {
		t.Fatal(err)
	}

	if err := db.svc.Detach(ctx, formsService, cover.ID, coreAttachment); !errors.Is(err, media.ErrAttachWrongService) {
		t.Fatalf("err = %v, want %v", err, media.ErrAttachWrongService)
	}
	attached(t, db.get(t, cover.ID))
}

// The expiry cleanup never purges a Media another product attached; once
// that product removes its last Media attachment, the Media goes 30 days
// later.
func TestPostgresExpiryCleanupKeepsWhatAProductAttached(t *testing.T) {
	db := newMediaDatabase(t)
	ctx := context.Background()
	logo := db.withBlob(t, "cms_image")
	page := db.attachFor(t, cmsService, logo, media.Owner{Service: "cms", Type: "page", ID: uuid.New()}, media.RoleImage)

	if _, err := media.PurgeExpired(ctx, db.store, db.blobs, time.Now().Add(48*time.Hour), nil); err != nil {
		t.Fatal(err)
	}
	if db.purged(t, logo) {
		t.Fatal("attached Media purged after its pending TTL")
	}

	if err := db.svc.Detach(ctx, cmsService, logo.ID, page.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := media.PurgeExpired(ctx, db.store, db.blobs, time.Now().Add(29*24*time.Hour), nil); err != nil {
		t.Fatal(err)
	}
	if db.purged(t, logo) {
		t.Fatal("detached Media purged inside its 30 days")
	}
	if _, err := media.PurgeExpired(ctx, db.store, db.blobs, time.Now().Add(31*24*time.Hour), nil); err != nil {
		t.Fatal(err)
	}
	if !db.purged(t, logo) {
		t.Fatal("detached Media kept past its 30 days")
	}
}
