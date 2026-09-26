package media_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/media"
)

func (d mediaDatabase) legacyReport(t *testing.T) media.LegacyReport {
	t.Helper()
	report, err := media.ReportLegacy(context.Background(), d.store)
	if err != nil {
		t.Fatal(err)
	}
	return report
}

func orphanIDs(report media.LegacyReport) []uuid.UUID {
	ids := make([]uuid.UUID, 0, len(report.Orphans))
	for _, orphan := range report.Orphans {
		ids = append(ids, orphan.ID)
	}
	return ids
}

// An orphan is a current legacy Media that nothing in core uses: never
// attached, or removed from its last record. Attached, purposed, archived
// (the archive window's business) and purged Media are not orphans.
func TestPostgresLegacyReportListsTheLegacyMediaNothingInCoreUses(t *testing.T) {
	db := newMediaDatabase(t)
	ctx := context.Background()
	never := db.storedBeforePurposes(t, "answer.png")
	dropped := db.storedBeforePurposes(t, "old-cover.png")
	cover := db.storedBeforePurposes(t, "cover.png")
	archived := db.storedBeforePurposes(t, "archived.png")
	purged := db.storedBeforePurposes(t, "purged.png")
	// A purposed upload nothing attached yet expires on its own.
	db.upload(t, "event_gallery")
	created, err := db.events().Create(ctx, db.organizer, event.Event{Name: "Hack", Location: "YTÜ", OwnerTeam: "WEBLAB", CoverImageID: &dropped.ID})
	if err != nil {
		t.Fatal(err)
	}
	created.CoverImageID = &cover.ID
	if _, err := db.events().Update(ctx, db.organizer, created.ID, created); err != nil {
		t.Fatal(err)
	}
	if err := db.store.Archive(ctx, archived.ID, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := db.pool.Exec(ctx, `UPDATE media SET deleted_at = now(), blob_purge_started_at = now(), blob_purged_at = now() WHERE id = $1`, purged.ID); err != nil {
		t.Fatal(err)
	}

	report := db.legacyReport(t)

	got := orphanIDs(report)
	if len(got) != 2 || got[0] != never.ID || got[1] != dropped.ID {
		t.Fatalf("orphans %v, want %v then %v (by upload time)", got, never.ID, dropped.ID)
	}
	// The cover is not an orphan: core attaches it. Until the purpose
	// backfill gives it event_cover it is counted as legacy core attaches.
	if report.AttachedByCore != 1 {
		t.Fatalf("legacy Media core attaches: %d, want 1 (the cover)", report.AttachedByCore)
	}
	orphan := report.Orphans[0]
	if orphan.Name != "answer.png" || orphan.Type != "image/png" || orphan.Size != int64(len(pngDot())) ||
		orphan.Key != never.Key || orphan.UploadedBy != never.UploadedBy || !orphan.CreatedAt.Equal(never.CreatedAt) {
		t.Fatalf("orphan %+v, want the record of %+v", orphan, never)
	}
}

// Safety net: a Media a core record links without its Media attachment is
// never an orphan, and the report counts such links. While the count is not
// zero the purge's hard-coded list of core links has to stay.
func TestPostgresLegacyReportNeverCallsAMediaACoreRecordLinksAnOrphan(t *testing.T) {
	db := newMediaDatabase(t)
	ctx := context.Background()
	cover := db.storedBeforePurposes(t, "cover.png")
	photo := db.storedBeforePurposes(t, "photo.png")
	if report := db.legacyReport(t); len(report.Orphans) != 2 || report.CoreLinksWithoutAttachment != 0 {
		t.Fatalf("before the links: %+v", report)
	}
	for _, statement := range []string{
		`ALTER TABLE events DISABLE TRIGGER events_media_attachments_insert`,
		`ALTER TABLE event_images DISABLE TRIGGER event_images_media_attachments_insert`,
	} {
		if _, err := db.pool.Exec(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	eventID := uuid.New()
	if _, err := db.pool.Exec(ctx, `INSERT INTO events (id, name, location, owner_team, cover_image_id) VALUES ($1, 'Unattached', 'YTÜ', 'WEBLAB', $2)`, eventID, cover.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.pool.Exec(ctx, `INSERT INTO event_images (event_id, media_id) VALUES ($1, $2)`, eventID, photo.ID); err != nil {
		t.Fatal(err)
	}

	report := db.legacyReport(t)

	if len(report.Orphans) != 0 {
		t.Fatalf("orphans %v, want none: an Event links both", orphanIDs(report))
	}
	if report.CoreLinksWithoutAttachment != 2 {
		t.Fatalf("core links without a Media attachment: %d, want 2", report.CoreLinksWithoutAttachment)
	}
}

func expireOrphans(t *testing.T, db mediaDatabase, ids []uuid.UUID, now time.Time, apply bool) media.LegacyExpiryReport {
	t.Helper()
	report, err := media.ExpireLegacyOrphans(context.Background(), db.store, ids, now, apply, func(err error) {
		t.Errorf("expiry failure: %v", err)
	})
	if err != nil {
		t.Fatal(err)
	}
	return report
}

// The switch Yusuf runs after reviewing the report (Q19): each reviewed Media
// that is still a legacy orphan gets 30 days, after which the expiry cleanup
// purges it. Without apply it only counts. Media no longer orphans are named
// and left alone, and a second run does not restart a window.
func TestPostgresLegacyOrphanExpiryStartsThirtyDaysOnlyForReviewedOrphans(t *testing.T) {
	db := newMediaDatabase(t)
	ctx := context.Background()
	orphan := db.withBlob(t, media.PurposeLegacy)
	reviewedThenUsed := db.withBlob(t, media.PurposeLegacy)
	notReviewed := db.withBlob(t, media.PurposeLegacy)
	purposed := db.withBlob(t, "event_gallery")
	missing := uuid.New()
	reviewed := []uuid.UUID{orphan.ID, reviewedThenUsed.ID, purposed.ID, missing}
	if _, err := db.events().Create(ctx, db.organizer, event.Event{Name: "Hack", Location: "YTÜ", OwnerTeam: "WEBLAB", CoverImageID: &reviewedThenUsed.ID}); err != nil {
		t.Fatal(err)
	}
	now := time.Now()

	dry := expireOrphans(t, db, reviewed, now, false)
	if dry.Expiring != 1 || dry.AlreadyExpiring != 0 || len(dry.NotOrphans) != 3 {
		t.Fatalf("dry run %+v", dry)
	}
	if got := db.get(t, orphan.ID); got.ExpiresAt != nil {
		t.Fatalf("a dry run set an expiry: %v", got.ExpiresAt)
	}

	applied := expireOrphans(t, db, reviewed, now, true)
	if applied.Expiring != 1 || len(applied.NotOrphans) != 3 {
		t.Fatalf("apply %+v", applied)
	}
	got := db.get(t, orphan.ID)
	if got.ExpiresAt == nil || !got.ExpiresAt.Equal(now.Add(30*24*time.Hour).Truncate(time.Microsecond)) || got.Purpose != media.PurposeLegacy {
		t.Fatalf("orphan purpose %q expires %v, want legacy for 30 days from %v", got.Purpose, got.ExpiresAt, now)
	}
	for _, kept := range []media.Media{reviewedThenUsed, notReviewed} {
		if got := db.get(t, kept.ID); got.ExpiresAt != nil {
			t.Fatalf("media %s expires %v", kept.ID, got.ExpiresAt)
		}
	}
	if got := db.get(t, purposed.ID); !got.ExpiresAt.Equal(*purposed.ExpiresAt) {
		t.Fatalf("the purposed Media's own expiry moved to %v", got.ExpiresAt)
	}

	again := expireOrphans(t, db, reviewed, now.Add(24*time.Hour), true)
	if again.Expiring != 0 || again.AlreadyExpiring != 1 {
		t.Fatalf("second run %+v", again)
	}
	if later := db.get(t, orphan.ID); !later.ExpiresAt.Equal(*got.ExpiresAt) {
		t.Fatalf("second run moved the window to %v", later.ExpiresAt)
	}

	// The expiry cleanup takes it from there, and only after the 30 days.
	if report, err := media.PurgeExpired(ctx, db.store, db.blobs, now.Add(29*24*time.Hour), nil); err != nil || db.purged(t, orphan) {
		t.Fatalf("purged before its 30 days: %+v %v", report, err)
	}
	if _, err := media.PurgeExpired(ctx, db.store, db.blobs, now.Add(31*24*time.Hour), nil); err != nil {
		t.Fatal(err)
	}
	if !db.purged(t, orphan) {
		t.Fatal("orphan kept after its 30 days")
	}
	for _, kept := range []media.Media{reviewedThenUsed, notReviewed} {
		if db.purged(t, kept) {
			t.Fatalf("media %s purged", kept.ID)
		}
	}
}
