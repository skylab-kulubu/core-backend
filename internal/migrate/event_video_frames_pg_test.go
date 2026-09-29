package migrate_test

import (
	"context"
	"errors"
	"io/fs"
	"regexp"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/skylab-kulubu/core-backend/db"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
)

const eventVideoFramesVersion = "20260929160000"

// framesHold reports whether the database knows a video's frame: its
// triggers run the frame's function, its foreign key lets a Media row go
// with only the frame, the role takes only video_frame (never legacy), and
// the worker's claim columns and index are there.
func framesHold(t *testing.T, pool *pgxpool.Pool) bool {
	t.Helper()
	var triggers int
	var setNull, frameFits, legacyFits, coverFits, dueIndexed, claimChecked bool
	err := pool.QueryRow(context.Background(), `SELECT
		(SELECT count(*) FROM pg_trigger
			WHERE tgname IN ('event_videos_frame_attachments_insert', 'event_videos_frame_attachments_update', 'event_videos_frame_attachments_delete')
			  AND tgfoid = to_regprocedure('public.sync_event_video_frame_attachments()')),
		COALESCE((SELECT confdeltype = 'n' FROM pg_constraint WHERE conname = 'event_videos_frame_media_id_fkey'), false),
		media_purpose_fits_role('core', 'event_video_frame', 'video_frame'),
		media_purpose_fits_role('core', 'event_video_frame', 'legacy'),
		media_purpose_fits_role('core', 'event_video_frame', 'event_cover'),
		to_regclass('public.event_videos_frame_due_idx') IS NOT NULL,
		EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'event_videos_frame_check')`,
		pgx.QueryExecModeSimpleProtocol).
		Scan(&triggers, &setNull, &frameFits, &legacyFits, &coverFits, &dueIndexed, &claimChecked)
	if err != nil {
		t.Fatal(err)
	}
	return triggers == 3 && setNull && frameFits && !legacyFits && !coverFits && dueIndexed && claimChecked
}

// frameAttachedAndDetached reports whether a frame written to a video is
// attached to the Event in its role, and detached once the video goes.
func frameAttachedAndDetached(t *testing.T, pool *pgxpool.Pool) bool {
	t.Helper()
	ctx := context.Background()
	eventID, videos := eventWithVideos(t, pool, 1)
	frame := posterMedia(t, pool, "video_frame", "image/jpeg")
	status := func() (string, int) {
		t.Helper()
		var status string
		var roles int
		if err := pool.QueryRow(ctx, `SELECT status, (SELECT count(*) FROM media_attachments
			WHERE media_id = $1 AND owner_type = 'event' AND owner_id = $2 AND role = 'event_video_frame') FROM media WHERE id = $1`,
			frame, eventID.String()).Scan(&status, &roles); err != nil {
			t.Fatal(err)
		}
		return status, roles
	}
	if _, err := pool.Exec(ctx, `UPDATE event_videos SET frame_media_id = $3 WHERE event_id = $1 AND media_id = $2`, eventID, videos[0], frame); err != nil {
		t.Fatal(err)
	}
	attached, roles := status()
	if _, err := pool.Exec(ctx, `DELETE FROM event_videos WHERE event_id = $1 AND media_id = $2`, eventID, videos[0]); err != nil {
		t.Fatal(err)
	}
	detached, left := status()
	return attached == "attached" && roles == 1 && detached == "detached" && left == 0
}

// frameChangeLocksTheEvent reports whether a frame written without the
// Event's lock (raw SQL) takes it.
func frameChangeLocksTheEvent(t *testing.T, pool *pgxpool.Pool) bool {
	t.Helper()
	ctx := context.Background()
	eventID, videos := eventWithVideos(t, pool, 1)
	frame := posterMedia(t, pool, "video_frame", "image/jpeg")
	writer, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Rollback(ctx)
	if _, err := writer.Exec(ctx, `UPDATE event_videos SET frame_media_id = $3 WHERE event_id = $1 AND media_id = $2`, eventID, videos[0], frame); err != nil {
		t.Fatal(err)
	}
	_, err = pool.Exec(ctx, `SELECT 1 FROM events WHERE id = $1 FOR NO KEY UPDATE NOWAIT`, eventID)
	var pgErr *pgconn.PgError
	if err != nil && !(errors.As(err, &pgErr) && pgErr.Code == "55P03") {
		t.Fatal(err)
	}
	return err != nil
}

// frameClaimsHold reports whether the claim columns keep a claim whole (its
// id and lease together), attempts counted from zero, and no state but
// failed.
func frameClaimsHold(t *testing.T, pool *pgxpool.Pool) bool {
	t.Helper()
	ctx := context.Background()
	eventID, videos := eventWithVideos(t, pool, 1)
	// Each write is rolled back: a damaged check must not leave a row the
	// repaired one refuses.
	set := func(assignments string) error {
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		_, err = tx.Exec(ctx, `UPDATE event_videos SET `+assignments+` WHERE event_id = $1 AND media_id = $2`, eventID, videos[0])
		return err
	}
	for _, ok := range []string{
		`frame_claim_id = gen_random_uuid(), frame_claimed_until = now()`,
		`frame_claim_id = NULL, frame_claimed_until = NULL, frame_attempts = 3, frame_retry_at = now()`,
		`frame_state = 'failed'`, `frame_state = NULL`,
	} {
		if err := set(ok); err != nil {
			t.Logf("%s refused: %v", ok, err)
			return false
		}
	}
	for _, refused := range []string{`frame_claim_id = gen_random_uuid()`, `frame_claimed_until = now()`, `frame_attempts = -1`, `frame_state = 'done'`} {
		if set(refused) == nil {
			t.Logf("%s was stored", refused)
			return false
		}
	}
	return true
}

// undoEventVideoFrames rolls back the frame migration, which builds on the
// posters' column and role tables, so that an older down migration can
// run; applying again brings it back.
func undoEventVideoFrames(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	down, err := fs.ReadFile(db.DownSQL, "migrations/"+eventVideoFramesVersion+"_event_video_frames.down.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(context.Background(), string(down)); err != nil {
		t.Fatalf("event video frames down: %v", err)
	}
	if _, err := pool.Exec(context.Background(), `DELETE FROM schema_migrations WHERE version = `+eventVideoFramesVersion); err != nil {
		t.Fatal(err)
	}
}

// frameFunctionWithoutEventLock is the frame function as the migration
// writes it, but without the step that locks the Events.
func frameFunctionWithoutEventLock(t *testing.T) string {
	t.Helper()
	up, err := fs.ReadFile(db.UpSQL, "migrations/"+eventVideoFramesVersion+"_event_video_frames.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	body := string(up)
	start := strings.Index(body, "CREATE OR REPLACE FUNCTION sync_event_video_frame_attachments()")
	end := strings.Index(body, "CREATE OR REPLACE TRIGGER event_videos_frame_attachments_insert")
	lock := regexp.MustCompile(`(?s)\n\s*PERFORM 1 FROM events.*?FOR NO KEY UPDATE;`)
	if start < 0 || end < start || !lock.MatchString(body[start:end]) {
		t.Fatal("the migration's frame function is not where the test looks for it")
	}
	return lock.ReplaceAllString(body[start:end], "")
}

// A database that lost part of the frames' schema, or whose role tables a
// rerun of the posters' migration put back (without the frame's role), is
// not taken for migrated: the migration runs again and puts it back.
func TestApplyRepairsTheVideoFrameLinks(t *testing.T) {
	posters, err := fs.ReadFile(db.UpSQL, "migrations/"+eventVideoPostersVersion+"_event_video_posters.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	for name, damage := range map[string]string{
		"a lost trigger":        `DROP TRIGGER event_videos_frame_attachments_delete ON event_videos`,
		"the older role tables": string(posters),
		"the poster's function": `CREATE OR REPLACE TRIGGER event_videos_frame_attachments_update AFTER UPDATE ON event_videos
			REFERENCING OLD TABLE AS old_owners NEW TABLE AS new_owners
			FOR EACH STATEMENT EXECUTE FUNCTION sync_event_video_poster_attachments()`,
		"a function that forgets the frame": `CREATE OR REPLACE FUNCTION sync_event_video_frame_attachments() RETURNS trigger
			LANGUAGE plpgsql AS $$ BEGIN RETURN NULL; END; $$`,
		"a function that does not lock the Event": frameFunctionWithoutEventLock(t),
		"a lost foreign key":                      `ALTER TABLE event_videos DROP CONSTRAINT event_videos_frame_media_id_fkey`,
		"a foreign key that takes the video's row": `ALTER TABLE event_videos DROP CONSTRAINT event_videos_frame_media_id_fkey;
			ALTER TABLE event_videos ADD CONSTRAINT event_videos_frame_media_id_fkey FOREIGN KEY (frame_media_id) REFERENCES media (id) ON DELETE CASCADE`,
		"a lost index":       `DROP INDEX event_videos_frame_media_id_idx`,
		"a lost due index":   `DROP INDEX event_videos_frame_due_idx`,
		"a lost claim check": `ALTER TABLE event_videos DROP CONSTRAINT event_videos_frame_check`,
		"a looser claim check": `ALTER TABLE event_videos DROP CONSTRAINT event_videos_frame_check;
			ALTER TABLE event_videos ADD CONSTRAINT event_videos_frame_check CHECK (frame_attempts >= 0)`,
	} {
		t.Run(name, func(t *testing.T) {
			pool := postgresPool(t)
			ctx := context.Background()
			if err := migrate.Apply(ctx, pool); err != nil {
				t.Fatal(err)
			}
			whole := func() bool {
				return framesHold(t, pool) && frameAttachedAndDetached(t, pool) && frameChangeLocksTheEvent(t, pool) && frameClaimsHold(t, pool) && framesIndexed(t, pool)
			}
			if !whole() {
				t.Fatal("a fresh database does not keep videos' frames")
			}
			if _, err := pool.Exec(ctx, `DELETE FROM schema_migrations WHERE version = `+eventVideoFramesVersion); err != nil {
				t.Fatal(err)
			}
			if _, err := pool.Exec(ctx, damage); err != nil {
				t.Fatal(err)
			}
			if whole() {
				t.Fatal("the damage broke nothing")
			}
			if err := migrate.Apply(ctx, pool); err != nil {
				t.Fatal(err)
			}
			if !whole() {
				t.Fatalf("%s was not repaired", name)
			}
		})
	}
}

// framesIndexed reports whether the index the purge reads frames by is
// there.
func framesIndexed(t *testing.T, pool *pgxpool.Pool) bool {
	t.Helper()
	var indexed bool
	if err := pool.QueryRow(context.Background(), `SELECT to_regclass('public.event_videos_frame_media_id_idx') IS NOT NULL`).Scan(&indexed); err != nil {
		t.Fatal(err)
	}
	return indexed
}

// The down migration refuses while a video has a frame. Once none is left
// it drops the frame and puts back the posters' role tables; applying again
// brings it back.
func TestEventVideoFramesDownRefusesWhileAVideoHasAFrame(t *testing.T) {
	pool := postgresPool(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	down, err := fs.ReadFile(db.DownSQL, "migrations/"+eventVideoFramesVersion+"_event_video_frames.down.sql")
	if err != nil {
		t.Fatal(err)
	}
	eventID, _ := eventWithVideos(t, pool, 1)
	frame := posterMedia(t, pool, "video_frame", "image/jpeg")
	if _, err := pool.Exec(ctx, `UPDATE event_videos SET frame_media_id = $2 WHERE event_id = $1`, eventID, frame); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, string(down)); err == nil || !strings.Contains(err.Error(), "event video frames down") {
		t.Fatalf("down while a video has a frame: %v", err)
	}
	if !framesHold(t, pool) {
		t.Fatal("a refused down changed the schema")
	}

	if _, err := pool.Exec(ctx, `UPDATE event_videos SET frame_media_id = NULL WHERE event_id = $1`, eventID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, string(down)); err != nil {
		t.Fatalf("down once no video has a frame: %v", err)
	}
	var columns, functions int
	var frameRoleKnown bool
	if err := pool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM information_schema.columns WHERE table_name = 'event_videos' AND column_name LIKE 'frame%'),
		(SELECT count(*) FROM pg_proc WHERE proname = 'sync_event_video_frame_attachments'),
		EXISTS (SELECT 1 FROM media_role_purposes() WHERE role = 'event_video_frame')`, pgx.QueryExecModeSimpleProtocol).
		Scan(&columns, &functions, &frameRoleKnown); err != nil || columns != 0 || functions != 0 || frameRoleKnown {
		t.Fatalf("after down: %d columns, %d functions, frame role known %v (err %v)", columns, functions, frameRoleKnown, err)
	}
	if !postersHold(t, pool) || !sharedPosterKept(t, pool) {
		t.Fatal("the frames' down took the posters with it")
	}
	if _, err := pool.Exec(ctx, `DELETE FROM schema_migrations WHERE version = `+eventVideoFramesVersion); err != nil {
		t.Fatal(err)
	}
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if !framesHold(t, pool) || !frameAttachedAndDetached(t, pool) {
		t.Fatal("applying again after down did not bring the frames back")
	}
}
