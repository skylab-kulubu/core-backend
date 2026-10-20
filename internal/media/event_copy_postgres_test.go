package media_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/event"
)

// A copied Event uses its source's cover too (ticket 06): the copy's link
// is written in the copy's transaction, by the trigger every cover link
// goes through, and the source keeps its own.
func TestPostgresEventCopyLinksTheCoverToo(t *testing.T) {
	db := newMediaDatabase(t)
	ctx := context.Background()
	events := event.NewService(event.NewPostgresStore(db.pool), authz.NewAuthorizer(authz.DefaultPolicy()))
	cover := db.upload(t, "event_cover")
	start := time.Now().Add(48 * time.Hour)
	source, err := events.Create(ctx, db.organizer, event.Event{Name: "Hack", Location: "YTÜ", OwnerTeam: "WEBLAB", StartDate: &start, CoverImageID: &cover.ID})
	if err != nil {
		t.Fatal(err)
	}
	next := start.AddDate(1, 0, 0)
	copied, _, err := events.Copy(ctx, db.organizer, source.ID, event.CopyInput{ID: uuid.New(), Name: "Hack 2", StartDate: &next})
	if err != nil {
		t.Fatal(err)
	}
	if copied.Event.CoverImageID == nil || *copied.Event.CoverImageID != cover.ID || len(copied.Skipped) != 0 {
		t.Fatalf("copy cover %v skipped %v", copied.Event.CoverImageID, copied.Skipped)
	}
	var owners int
	if err := db.pool.QueryRow(ctx, `SELECT count(*) FROM media_attachments WHERE media_id = $1 AND owner_id IN ($2, $3)`,
		cover.ID, source.ID.String(), copied.Event.ID.String()).Scan(&owners); err != nil {
		t.Fatal(err)
	}
	if owners != 2 {
		t.Fatalf("cover attached to %d of the two Events", owners)
	}
	attached(t, db.get(t, cover.ID))
}
