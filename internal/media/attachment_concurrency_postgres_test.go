package media_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/media"
	"github.com/skylab-kulubu/core-backend/internal/testpostgres"
)

// Two transactions each remove one of the last two Media attachments of the
// same Media. The second waits for the first, then sees that none is left:
// the Media ends detached, not attached with nothing attaching it.
func TestPostgresRemovingTheLastTwoMediaAttachmentsAtOnceDetachesTheMedia(t *testing.T) {
	db := newMediaDatabase(t)
	ctx := context.Background()
	events := event.NewService(event.NewPostgresStore(db.pool), authz.NewAuthorizer(authz.DefaultPolicy()))
	photo := db.upload(t, "event_gallery")
	var eventIDs []uuid.UUID
	for range 2 {
		created, err := events.Create(ctx, db.organizer, event.Event{Name: "Hack", Location: "YTÜ", OwnerTeam: "WEBLAB"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := events.AddImages(ctx, db.organizer, created.ID, []uuid.UUID{photo.ID}); err != nil {
			t.Fatal(err)
		}
		eventIDs = append(eventIDs, created.ID)
	}

	first, err := db.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Rollback(ctx)
	if _, err := first.Exec(ctx, `DELETE FROM event_images WHERE event_id = $1`, eventIDs[0]); err != nil {
		t.Fatal(err)
	}
	second := make(chan error, 1)
	go func() {
		tx, err := db.pool.Begin(ctx)
		if err != nil {
			second <- err
			return
		}
		defer tx.Rollback(ctx)
		if _, err := tx.Exec(ctx, `DELETE FROM event_images WHERE event_id = $1 /* second */`, eventIDs[1]); err != nil {
			second <- err
			return
		}
		second <- tx.Commit(ctx)
	}()
	testpostgres.WaitForBlockedQuery(t, db.pool, "/* second */")
	if err := first.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-second; err != nil {
		t.Fatal(err)
	}

	if got := db.get(t, photo.ID); got.Status != media.StatusDetached || got.ExpiresAt == nil {
		t.Fatalf("status %q expires %v, want detached with its window", got.Status, got.ExpiresAt)
	}
}

// A transaction that only refers to a Media by foreign key (as every link
// does while it is written) does not hold up linking the same Media
// elsewhere. Were it to, two links of one Media written at once would each
// wait for the other.
func TestPostgresLinkingAMediaDoesNotWaitForAForeignKeyReference(t *testing.T) {
	db := newMediaDatabase(t)
	ctx := context.Background()
	events := event.NewService(event.NewPostgresStore(db.pool), authz.NewAuthorizer(authz.DefaultPolicy()))
	photo := db.upload(t, "event_gallery")
	created, err := events.Create(ctx, db.organizer, event.Event{Name: "Hack", Location: "YTÜ", OwnerTeam: "WEBLAB"})
	if err != nil {
		t.Fatal(err)
	}

	reference, err := db.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer reference.Rollback(ctx)
	if _, err := reference.Exec(ctx, `SELECT 1 FROM media WHERE id = $1 FOR KEY SHARE`, photo.ID); err != nil {
		t.Fatal(err)
	}

	linked := make(chan error, 1)
	go func() {
		_, err := events.AddImages(ctx, db.organizer, created.ID, []uuid.UUID{photo.ID})
		linked <- err
	}()
	select {
	case err := <-linked:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("linking waited for a transaction that only refers to the Media")
	}
	attached(t, db.get(t, photo.ID))
}
