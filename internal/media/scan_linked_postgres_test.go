package media_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/media"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// heldImage stores an image Media as one waiting for its scan (or rejected
// by it) is stored: held under pending/scan/. No reviewed purpose can be one
// (a public purpose that needs a scan is sent by Direct upload, which takes
// no image), so it is stored directly: the state the records' reads must
// handle whatever the catalogue becomes.
func (d mediaDatabase) heldImage(t *testing.T, purpose string, status media.Status) media.Media {
	t.Helper()
	m := media.Media{
		Name: "photo.jpg", Type: "image/jpeg", Kind: media.KindImage, Key: "pending/scan/" + uuid.NewString(),
		UploadedBy: d.uploader(), Purpose: purpose, Status: media.StatusScanning, Width: 1600, Height: 1200,
		SizeObjects: map[string]media.SizeObject{media.SizeCard: {ImageSize: media.ImageSize{Width: 400, Height: 300}, Type: "image/jpeg"}},
	}
	created, err := d.store.Create(context.Background(), m)
	if err != nil {
		t.Fatal(err)
	}
	if status == media.StatusRejected {
		if _, err := d.pool.Exec(context.Background(), `UPDATE media SET status = 'rejected', scan_result = 'infected', blob_purge_started_at = now(), blob_purged_at = now() WHERE id = $1`, created.ID); err != nil {
			t.Fatal(err)
		}
	}
	return created
}

// A record that links a Media waiting for its scan builds no address for
// it: no Event cover, gallery image or profile picture address, and no
// sizes, so a held key never leaks.
func TestPostgresRecordsBuildNoAddressForAScanningMedia(t *testing.T) {
	d := newMediaDatabase(t)
	ctx := context.Background()
	cover := d.heldImage(t, media.PurposeEventCover, media.StatusScanning)
	photo := d.heldImage(t, media.PurposeEventGallery, media.StatusScanning)
	created, err := d.events().Create(ctx, d.organizer, event.Event{Name: "Hack", Location: "YTÜ", OwnerTeam: "WEBLAB", CoverImageID: &cover.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.events().AddImages(ctx, d.organizer, created.ID, []uuid.UUID{photo.ID}); err != nil {
		t.Fatal(err)
	}
	got, err := d.events().Get(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.CoverImageURL != "" || got.CoverImageSizes != nil {
		t.Fatalf("cover %q with sizes %v", got.CoverImageURL, got.CoverImageSizes)
	}
	if len(got.Images) != 1 || got.Images[0].URL != "" || got.Images[0].Sizes != nil || len(got.ImageURLs) != 0 {
		t.Fatalf("gallery %+v, urls %v", got.Images, got.ImageURLs)
	}
	if resource := got.Resource(); resource.CoverImageURL != "" || resource.CoverImageSizes != nil {
		t.Fatalf("resource cover %q with sizes %v", resource.CoverImageURL, resource.CoverImageSizes)
	}

	users := user.NewPostgresStore(d.pool)
	picture := d.heldImage(t, media.PurposeProfilePicture, media.StatusScanning)
	if _, err := user.NewService(users).SetProfilePicture(ctx, d.uploader(), picture.ID, picture.Key); err != nil {
		t.Fatal(err)
	}
	person, err := users.Get(ctx, d.uploader())
	if err != nil {
		t.Fatal(err)
	}
	if person.ProfilePictureURL != "" || media.ConfiguredAddresses().LinkedSizes(person.ProfilePicture) != nil {
		t.Fatalf("profile picture %q", person.ProfilePictureURL)
	}

	// The same reads for a rejected Media (its object deleted).
	rejected := d.heldImage(t, media.PurposeEventCover, media.StatusRejected)
	var image *media.LinkedImage
	if err := d.pool.QueryRow(ctx, `SELECT `+media.LinkedImageSQL("m")+` FROM media m WHERE m.id = $1`, rejected.ID).Scan(&image); err != nil {
		t.Fatal(err)
	}
	if image.Key() != "" || media.ConfiguredAddresses().LinkedSizes(image) != nil {
		t.Fatalf("rejected Media: key %q", image.Key())
	}
}
