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

// A video's poster is one of core's own links too (media redesign ticket
// 24): a poster whose Media attachment is missing is counted by the legacy
// report, and its Media is not purged while a video shows it. Once cleared,
// it is.
func TestPostgresAVideosPosterIsACoreLink(t *testing.T) {
	db := newMediaDatabase(t)
	ctx := context.Background()
	video, err := db.store.Create(ctx, media.Media{
		Name: "a.mp4", Type: "video/mp4", Kind: media.KindFile, Key: "videos/" + uuid.NewString() + ".mp4", UploadedBy: db.uploader(), Purpose: media.PurposeVideo,
	})
	if err != nil {
		t.Fatal(err)
	}
	poster, err := db.store.Create(ctx, media.Media{
		Name: "kapak.jpg", Type: "image/jpeg", Kind: media.KindImage, Key: "images/" + uuid.NewString(), UploadedBy: db.uploader(), Purpose: media.PurposeEventCover,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.blobs.Put(ctx, poster.Key, []byte("x"), media.BlobMetadata{ContentType: "image/jpeg"}); err != nil {
		t.Fatal(err)
	}
	eventID := uuid.New()
	if _, err := db.pool.Exec(ctx, `INSERT INTO events (id, name, location, owner_team) VALUES ($1, 'Unattached', 'YTÜ', 'WEBLAB')`, eventID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.pool.Exec(ctx, `INSERT INTO event_videos (event_id, media_id, order_index) VALUES ($1, $2, 1)`, eventID, video.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.pool.Exec(ctx, `ALTER TABLE event_videos DISABLE TRIGGER event_videos_poster_attachments_update`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.pool.Exec(ctx, `UPDATE event_videos SET poster_media_id = $2 WHERE event_id = $1`, eventID, poster.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.pool.Exec(ctx, `ALTER TABLE event_videos ENABLE TRIGGER event_videos_poster_attachments_update`); err != nil {
		t.Fatal(err)
	}

	if report := db.legacyReport(t); report.CoreLinksWithoutAttachment != 1 {
		t.Fatalf("core links without a Media attachment: %d, want the poster", report.CoreLinksWithoutAttachment)
	}
	uploader := db.uploader()
	if err := db.store.Archive(ctx, poster.ID, &uploader); err != nil {
		t.Fatal(err)
	}
	if _, err := db.pool.Exec(ctx, `UPDATE media SET deleted_at = $2 WHERE id = $1`, poster.ID, time.Now().UTC().Add(-31*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	report, err := media.PurgeDeleted(ctx, db.store, db.blobs, time.Now().UTC(), media.DefaultBlobRecoveryWindow, 25)
	if err != nil {
		t.Fatal(err)
	}
	if report.Purged != 0 || report.Referenced != 1 || db.purged(t, poster) {
		t.Fatalf("purge %+v, want the poster kept as used", report)
	}

	if _, err := db.pool.Exec(ctx, `UPDATE event_videos SET poster_media_id = NULL WHERE event_id = $1`, eventID); err != nil {
		t.Fatal(err)
	}
	if report, err := media.PurgeDeleted(ctx, db.store, db.blobs, time.Now().UTC().Add(time.Minute), media.DefaultBlobRecoveryWindow, 25); err != nil || report.Purged != 1 {
		t.Fatalf("purge once the video let it go: %+v, err %v", report, err)
	}
}

// The database checks a poster as it checks every new link, whoever writes
// it: a Media uploaded without a purpose, or of a purpose a cover does not
// take, is refused under the poster's role (media_attachment_purpose_fits),
// and nothing is written; a gallery photo is a poster.
func TestPostgresTheDatabaseRefusesAPosterACoverWouldNotTake(t *testing.T) {
	db := newMediaDatabase(t)
	ctx := context.Background()
	stored := func(purpose string) uuid.UUID {
		t.Helper()
		m, err := db.store.Create(ctx, media.Media{
			Name: "x.png", Type: "image/png", Kind: media.KindImage, Key: "images/" + uuid.NewString(), UploadedBy: db.uploader(), Purpose: purpose,
		})
		if err != nil {
			t.Fatal(err)
		}
		return m.ID
	}
	video, err := db.store.Create(ctx, media.Media{
		Name: "a.mp4", Type: "video/mp4", Kind: media.KindFile, Key: "videos/" + uuid.NewString() + ".mp4", UploadedBy: db.uploader(), Purpose: media.PurposeVideo,
	})
	if err != nil {
		t.Fatal(err)
	}
	eventID := uuid.New()
	if _, err := db.pool.Exec(ctx, `INSERT INTO events (id, name, location, owner_team) VALUES ($1, 'Hack', 'YTÜ', 'WEBLAB')`, eventID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.pool.Exec(ctx, `INSERT INTO event_videos (event_id, media_id, order_index) VALUES ($1, $2, 1)`, eventID, video.ID); err != nil {
		t.Fatal(err)
	}
	for _, purpose := range []string{media.PurposeLegacy, media.PurposeProfilePicture, media.PurposeCMSImage} {
		poster := stored(purpose)
		_, err := db.pool.Exec(ctx, `UPDATE event_videos SET poster_media_id = $2 WHERE event_id = $1`, eventID, poster)
		refusal, ok := media.DatabaseLinkRefusal(err)
		if !ok || refusal.MediaID != poster || refusal.Role != media.RoleEventVideoPoster {
			t.Fatalf("a %s poster: %v, want refused in the poster's role", purpose, err)
		}
	}
	gallery := stored(media.PurposeEventGallery)
	if _, err := db.pool.Exec(ctx, `UPDATE event_videos SET poster_media_id = $2 WHERE event_id = $1`, eventID, gallery); err != nil {
		t.Fatal(err)
	}
	var role string
	if err := db.pool.QueryRow(ctx, `SELECT role FROM media_attachments WHERE media_id = $1 AND owner_service = 'core' AND owner_type = 'event' AND owner_id = $2`,
		gallery, eventID.String()).Scan(&role); err != nil || role != string(media.RoleEventVideoPoster) {
		t.Fatalf("the gallery photo's Media attachment: role %q, err %v", role, err)
	}
}
