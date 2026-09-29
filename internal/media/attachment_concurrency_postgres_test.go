package media_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
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

// posterRace is an Event listing two videos, and a photo for their posters.
type posterRace struct {
	event  uuid.UUID
	videos [2]uuid.UUID
	poster media.Media
}

func newPosterRace(t *testing.T, db mediaDatabase) posterRace {
	t.Helper()
	ctx := context.Background()
	race := posterRace{event: uuid.New(), poster: db.upload(t, "event_cover")}
	if _, err := db.pool.Exec(ctx, `INSERT INTO events (id, name, location, owner_team) VALUES ($1, 'Hack', 'YTÜ', 'WEBLAB')`, race.event); err != nil {
		t.Fatal(err)
	}
	for i := range race.videos {
		video, err := db.store.Create(ctx, media.Media{
			Name: "a.mp4", Type: "video/mp4", Kind: media.KindFile, Key: "videos/" + uuid.NewString() + ".mp4",
			UploadedBy: uuid.MustParse(db.organizer.ID), Purpose: media.PurposeVideo,
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.pool.Exec(ctx, `INSERT INTO event_videos (event_id, media_id, order_index) VALUES ($1, $2, $3)`, race.event, video.ID, i+1); err != nil {
			t.Fatal(err)
		}
		race.videos[i] = video.ID
	}
	return race
}

// setPoster is the raw statement that gives video i of the race the poster
// (nil: none), as a writer that takes no Event lock of its own writes it.
func (r posterRace) setPoster(i int, poster *uuid.UUID, marker string) (string, []any) {
	return `UPDATE event_videos SET poster_media_id = $3 WHERE event_id = $1 AND media_id = $2 /* ` + marker + ` */`,
		[]any{r.event, r.videos[i], poster}
}

// Two transactions change the posters of two videos of one Event at once,
// neither locking the Event first (raw SQL, as maintenance or a cascade
// writes): the poster trigger takes the Event's lock itself, so the second
// waits for the first and then sees what it wrote. Two videos that both let
// go of their shared poster leave no Media attachment behind: the photo is
// detached. A video that takes the photo while the other gives it up leaves
// it attached, with its Media attachment.
func TestPostgresPosterChangesAtOnceKeepTheMediaAttachmentTrue(t *testing.T) {
	db := newMediaDatabase(t)
	ctx := context.Background()
	type write struct {
		video int
		shows bool
	}
	for name, tc := range map[string]struct {
		before        [2]bool // which videos show the poster before
		first, second write
		attachments   int
		status        media.Status
	}{
		"both let it go":                {before: [2]bool{true, true}, first: write{0, false}, second: write{1, false}, attachments: 0, status: media.StatusDetached},
		"one takes it, one gives it up": {before: [2]bool{true, false}, first: write{1, true}, second: write{0, false}, attachments: 1, status: media.StatusAttached},
	} {
		t.Run(name, func(t *testing.T) {
			race := newPosterRace(t, db)
			posterOf := func(shows bool) *uuid.UUID {
				if shows {
					return &race.poster.ID
				}
				return nil
			}
			for i, shows := range tc.before {
				statement, args := race.setPoster(i, posterOf(shows), "setup")
				if _, err := db.pool.Exec(ctx, statement, args...); err != nil {
					t.Fatal(err)
				}
			}

			first, err := db.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer first.Rollback(ctx)
			statement, args := race.setPoster(tc.first.video, posterOf(tc.first.shows), "first")
			if _, err := first.Exec(ctx, statement, args...); err != nil {
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
				statement, args := race.setPoster(tc.second.video, posterOf(tc.second.shows), "second")
				if _, err := tx.Exec(ctx, statement, args...); err != nil {
					second <- err
					return
				}
				second <- tx.Commit(ctx)
			}()
			// The second either waits for the first's Event lock or, without
			// one, is done already: commit the first either way.
			secondDone := waitBlockedOrDone(t, db.pool, "/* second */", second)
			if err := first.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			if err := secondDone(); err != nil {
				t.Fatal(err)
			}

			var attachments int
			if err := db.pool.QueryRow(ctx, `SELECT count(*) FROM media_attachments
				WHERE media_id = $1 AND owner_service = 'core' AND owner_type = 'event' AND owner_id = $2 AND role = 'event_video_poster'`,
				race.poster.ID, race.event.String()).Scan(&attachments); err != nil {
				t.Fatal(err)
			}
			if got := db.get(t, race.poster.ID); attachments != tc.attachments || got.Status != tc.status {
				t.Fatalf("the poster has %d Media attachments and is %s, want %d and %s", attachments, got.Status, tc.attachments, tc.status)
			}
		})
	}
}

// waitBlockedOrDone waits until the query containing fragment waits for a
// lock, or until done answers (the query did not wait); it returns what
// done answers, waiting for it when it has not yet.
func waitBlockedOrDone(t *testing.T, pool *pgxpool.Pool, fragment string, done <-chan error) func() error {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		select {
		case err := <-done:
			return func() error { return err }
		default:
		}
		var blocked bool
		if err := pool.QueryRow(context.Background(), `SELECT EXISTS (
				SELECT 1 FROM pg_stat_activity
				WHERE datname = current_database() AND pid <> pg_backend_pid()
				  AND wait_event_type = 'Lock' AND query LIKE '%' || $1 || '%')`, fragment).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if blocked {
			return func() error { return <-done }
		}
		if time.Now().After(deadline) {
			t.Fatalf("the query containing %q neither waited nor finished", fragment)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
