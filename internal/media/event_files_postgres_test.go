package media_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/media"
)

// An Event's files and videos are core's own links (media redesign ticket
// 22), and the purge's safety net and the legacy report read them beside
// the Media attachments: a file or video whose Media attachment is missing
// is counted, and its Media is not purged while the Event lists it. Once
// the Event lets it go, it is.
func TestPostgresAnEventsFilesAndVideosAreCoreLinks(t *testing.T) {
	db := newMediaDatabase(t)
	ctx := context.Background()
	stored := func(purpose, contentType string) media.Media {
		t.Helper()
		item, err := db.store.Create(ctx, media.Media{
			Name: "x", Type: contentType, Kind: media.KindFile, Key: "files/" + uuid.NewString(), UploadedBy: db.uploader(), Purpose: purpose,
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := db.blobs.Put(ctx, item.Key, []byte("x"), media.BlobMetadata{ContentType: contentType}); err != nil {
			t.Fatal(err)
		}
		return item
	}
	file := stored(media.PurposeClubFile, "application/pdf")
	video := stored(media.PurposeVideo, "video/mp4")
	for _, statement := range []string{
		`ALTER TABLE event_files DISABLE TRIGGER event_files_media_attachments_insert`,
		`ALTER TABLE event_videos DISABLE TRIGGER event_videos_media_attachments_insert`,
	} {
		if _, err := db.pool.Exec(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	eventID := uuid.New()
	if _, err := db.pool.Exec(ctx, `INSERT INTO events (id, name, location, owner_team) VALUES ($1, 'Unattached', 'YTÜ', 'WEBLAB')`, eventID); err != nil {
		t.Fatal(err)
	}
	for table, item := range map[string]media.Media{"event_files": file, "event_videos": video} {
		if _, err := db.pool.Exec(ctx, `INSERT INTO `+table+` (event_id, media_id, order_index) VALUES ($1, $2, 1)`, eventID, item.ID); err != nil {
			t.Fatal(err)
		}
	}

	if report := db.legacyReport(t); report.CoreLinksWithoutAttachment != 2 {
		t.Fatalf("core links without a Media attachment: %d, want the file and the video", report.CoreLinksWithoutAttachment)
	}
	uploader := db.uploader()
	archivedAt := time.Now().UTC().Add(-31 * 24 * time.Hour)
	for _, item := range []media.Media{file, video} {
		if err := db.store.Archive(ctx, item.ID, &uploader); err != nil {
			t.Fatal(err)
		}
		if _, err := db.pool.Exec(ctx, `UPDATE media SET deleted_at = $2 WHERE id = $1`, item.ID, archivedAt); err != nil {
			t.Fatal(err)
		}
	}
	report, err := media.PurgeDeleted(ctx, db.store, db.blobs, time.Now().UTC(), media.DefaultBlobRecoveryWindow, 25)
	if err != nil {
		t.Fatal(err)
	}
	if report.Purged != 0 || report.Referenced != 2 {
		t.Fatalf("purge %+v, want the file and the video kept as used", report)
	}
	for _, item := range []media.Media{file, video} {
		if db.purged(t, item) {
			t.Fatalf("%s was purged while the Event lists it", item.Type)
		}
	}

	for _, table := range []string{"event_files", "event_videos"} {
		if _, err := db.pool.Exec(ctx, `DELETE FROM `+table+` WHERE event_id = $1`, eventID); err != nil {
			t.Fatal(err)
		}
	}
	if report, err := media.PurgeDeleted(ctx, db.store, db.blobs, time.Now().UTC().Add(time.Minute), media.DefaultBlobRecoveryWindow, 25); err != nil || report.Purged != 2 {
		t.Fatalf("purge once the Event let them go: %+v, err %v", report, err)
	}
}
