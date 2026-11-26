package event_test

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// The store keeps an Event's theme and ticket colour, reads a key the
// catalogue lost as Kurumsal, reads its poster (its own ticket image) for
// the mails, and a copy carries the theme, the colour and the ticket image:
// a cover region with the cover it was drawn on, an image of its own when
// the copy's Owner team may use it (docs/event-theme.md, docs/event-copy.md).
func TestPostgresEventTheme(t *testing.T) {
	d := newCopyDatabase(t)
	ctx := context.Background()
	key, color := "artlab", "#FF8A1F"
	src := d.source
	src.Theme, src.TicketAccentColor = &key, &color
	updated, err := d.store.Update(ctx, src)
	if err != nil || updated.ThemeKey() != "artlab" || updated.TicketAccentColor == nil || *updated.TicketAccentColor != color {
		t.Fatalf("update %v %v %v", updated.Theme, updated.TicketAccentColor, err)
	}
	if updated.BannerURL() != "" {
		t.Fatalf("a banner without a ticket image: %q", updated.BannerURL())
	}
	uploader := uuid.New()
	if _, _, err := user.NewService(user.NewPostgresStore(d.pool)).Ensure(ctx, uploader, user.Profile{Email: "yukleyen@example.test"}); err != nil {
		t.Fatal(err)
	}
	image := func(purpose, contentType string) uuid.UUID {
		id := uuid.New()
		if _, err := d.pool.Exec(ctx, `
			INSERT INTO media (id, file_name, file_type, file_url, file_size, uploaded_by, kind, purpose, status, visibility, width, height)
			VALUES ($1, 'afis', $2, $3, 10, $4, 'IMAGE', $5, 'pending', 'public', 900, 300)`,
			id, contentType, "images/"+id.String(), uploader, purpose); err != nil {
			t.Fatal(err)
		}
		return id
	}
	cover, poster := image("event_cover", "image/jpeg"), image("event_gallery", "image/jpeg")
	src = updated
	src.CoverImageID = &cover
	if _, err := d.store.Update(ctx, src); err != nil {
		t.Fatal(err)
	}
	if _, err := d.store.AddImages(ctx, src.ID, []uuid.UUID{poster}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.pool.Exec(ctx, `INSERT INTO event_ticket_images (event_id, source, media_id, crop_x, crop_y, crop_w, crop_h)
		VALUES ($1, 'upload', $2, 0, 0.2, 1, 0.13)`, src.ID, poster); err != nil {
		t.Fatal(err)
	}
	read, err := d.store.Get(ctx, src.ID)
	if err != nil {
		t.Fatal(err)
	}
	// The poster has no page size yet: its original, an https JPEG.
	if got := event.MailLook(read); got["Theme"] != "artlab" || !strings.HasPrefix(got["EventBannerUrl"], "https://") || !strings.HasSuffix(got["EventBannerUrl"], "images/"+poster.String()) {
		t.Fatalf("mail look %v", got)
	}
	start := time.Date(2027, 3, 14, 16, 0, 0, 0, time.UTC)
	copied, _, err := d.svc.Copy(ctx, d.yk, src.ID, event.CopyInput{ID: uuid.New(), Name: "ARTLAB 2027", StartDate: &start})
	if err != nil {
		t.Fatal(err)
	}
	if copied.Event.ThemeKey() != "artlab" || copied.Event.TicketAccentColor == nil || *copied.Event.TicketAccentColor != color || slices.Contains(copied.Skipped, event.SkippedTicketImage) {
		t.Fatalf("copy %v %v %v", copied.Event.Theme, copied.Event.TicketAccentColor, copied.Skipped)
	}
	if n := d.count(t, `SELECT count(*) FROM event_ticket_images WHERE event_id = $1 AND source = 'upload' AND media_id = $2 AND crop_y = 0.2`, copied.Event.ID, poster); n != 1 {
		t.Fatalf("the copy's ticket image: %d rows", n)
	}
	// A cover region comes along with the cover it was drawn on.
	if _, err := d.pool.Exec(ctx, `UPDATE event_ticket_images SET source = 'cover', media_id = NULL, cover_media_id = $2 WHERE event_id = $1`, src.ID, cover); err != nil {
		t.Fatal(err)
	}
	region, _, err := d.svc.Copy(ctx, d.yk, src.ID, event.CopyInput{ID: uuid.New(), Name: "ARTLAB 2028", StartDate: &start})
	if err != nil {
		t.Fatal(err)
	}
	if n := d.count(t, `SELECT count(*) FROM event_ticket_images WHERE event_id = $1 AND source = 'cover' AND cover_media_id = $2`, region.Event.ID, cover); n != 1 {
		t.Fatalf("the copy's cover region: %d rows", n)
	}
	// Another team may not use this team's image (the Team media library):
	// the image is left out and named.
	if _, err := d.pool.Exec(ctx, `UPDATE event_ticket_images SET source = 'upload', media_id = $2, cover_media_id = NULL WHERE event_id = $1`, src.ID, poster); err != nil {
		t.Fatal(err)
	}
	other, _, err := d.svc.Copy(ctx, d.yk, src.ID, event.CopyInput{ID: uuid.New(), Name: "ALGOLAB'a", StartDate: &start, OwnerTeam: "ALGOLAB"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(other.Skipped, event.SkippedTicketImage) || d.count(t, `SELECT count(*) FROM event_ticket_images WHERE event_id = $1`, other.Event.ID) != 0 {
		t.Fatalf("into another team: skipped %v", other.Skipped)
	}
	// A key the catalogue no longer has reads as Kurumsal.
	if _, err := d.pool.Exec(ctx, `UPDATE events SET theme = 'artlab-2019' WHERE id = $1`, src.ID); err != nil {
		t.Fatal(err)
	}
	if lost, err := d.store.Get(ctx, src.ID); err != nil || lost.Theme != nil || lost.ThemeKey() != "kurumsal" {
		t.Fatalf("a lost key %v %v", lost.Theme, err)
	}
}
