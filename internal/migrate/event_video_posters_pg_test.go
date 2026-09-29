package migrate_test

import (
	"context"
	"errors"
	"io/fs"
	"regexp"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/skylab-kulubu/core-backend/db"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

const eventVideoPostersVersion = "20260929140000"

// postersHold reports whether the database knows a video's poster: its
// triggers run the poster's function, its foreign key lets a Media row go
// with only the poster, the role takes both Event photo purposes, and a
// legacy Media does not fit it.
func postersHold(t *testing.T, pool *pgxpool.Pool) bool {
	t.Helper()
	var triggers int
	var setNull, coverFits, galleryFits, legacyFits bool
	// Planned afresh each time (the simple protocol), as eventFilesRolesHold.
	err := pool.QueryRow(context.Background(), `SELECT
		(SELECT count(*) FROM pg_trigger
			WHERE tgname IN ('event_videos_poster_attachments_insert', 'event_videos_poster_attachments_update', 'event_videos_poster_attachments_delete')
			  AND tgfoid = to_regprocedure('public.sync_event_video_poster_attachments()')),
		COALESCE((SELECT confdeltype = 'n' FROM pg_constraint WHERE conname = 'event_videos_poster_media_id_fkey'), false),
		media_purpose_fits_role('core', 'event_video_poster', 'event_cover'),
		media_purpose_fits_role('core', 'event_video_poster', 'event_gallery'),
		media_purpose_fits_role('core', 'event_video_poster', 'legacy')`,
		pgx.QueryExecModeSimpleProtocol).
		Scan(&triggers, &setNull, &coverFits, &galleryFits, &legacyFits)
	if err != nil {
		t.Fatal(err)
	}
	return triggers == 3 && setNull && coverFits && galleryFits && !legacyFits
}

// posterMedia stores a Media of the purpose, attached to nothing yet.
func posterMedia(t *testing.T, pool *pgxpool.Pool, purpose, contentType string) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	uploader := uuid.New()
	if _, _, err := user.NewService(user.NewPostgresStore(pool)).Ensure(ctx, uploader, user.Profile{Email: uploader.String() + "@example.test"}); err != nil {
		t.Fatal(err)
	}
	id := uuid.New()
	if _, err := pool.Exec(ctx, `
		INSERT INTO media (id, file_name, file_type, file_url, file_size, uploaded_by, kind, purpose, status)
		VALUES ($1, 'x', $2, $3, 10, $4, 'FILE', $5, 'pending')`, id, contentType, "files/"+id.String(), uploader, purpose); err != nil {
		t.Fatal(err)
	}
	return id
}

// eventWithVideos is an Event listing n videos, and their ids.
func eventWithVideos(t *testing.T, pool *pgxpool.Pool, n int) (uuid.UUID, []uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	eventID := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO events (id, name, location, owner_team) VALUES ($1, 'Hack', 'YTÜ', 'WEBLAB')`, eventID); err != nil {
		t.Fatal(err)
	}
	videos := make([]uuid.UUID, 0, n)
	for i := range n {
		video := posterMedia(t, pool, "video", "video/mp4")
		if _, err := pool.Exec(ctx, `INSERT INTO event_videos (event_id, media_id, order_index) VALUES ($1, $2, $3)`, eventID, video, i+1); err != nil {
			t.Fatal(err)
		}
		videos = append(videos, video)
	}
	return eventID, videos
}

// sharedPosterKept reports whether a poster two videos show stays attached
// when one of them lets it go, and is detached with the last.
func sharedPosterKept(t *testing.T, pool *pgxpool.Pool) bool {
	t.Helper()
	ctx := context.Background()
	eventID, videos := eventWithVideos(t, pool, 2)
	poster := posterMedia(t, pool, "event_cover", "image/png")
	status := func() string {
		t.Helper()
		var status string
		if err := pool.QueryRow(ctx, `SELECT status FROM media WHERE id = $1`, poster).Scan(&status); err != nil {
			t.Fatal(err)
		}
		return status
	}
	if _, err := pool.Exec(ctx, `UPDATE event_videos SET poster_media_id = $2 WHERE event_id = $1`, eventID, poster); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE event_videos SET poster_media_id = NULL WHERE event_id = $1 AND media_id = $2`, eventID, videos[0]); err != nil {
		t.Fatal(err)
	}
	kept := status() == "attached"
	if _, err := pool.Exec(ctx, `DELETE FROM event_videos WHERE event_id = $1 AND media_id = $2`, eventID, videos[1]); err != nil {
		t.Fatal(err)
	}
	return kept && status() == "detached"
}

// postersIndexed reports whether the index the purge and the Team media
// library read posters by is there.
func postersIndexed(t *testing.T, pool *pgxpool.Pool) bool {
	t.Helper()
	var indexed bool
	if err := pool.QueryRow(context.Background(), `SELECT to_regclass('public.event_videos_poster_media_id_idx') IS NOT NULL`).Scan(&indexed); err != nil {
		t.Fatal(err)
	}
	return indexed
}

// posterChangeLocksTheEvent reports whether a poster written without the
// Event's lock (raw SQL) takes it: until the writer commits, no one else
// can lock the Event.
func posterChangeLocksTheEvent(t *testing.T, pool *pgxpool.Pool) bool {
	t.Helper()
	ctx := context.Background()
	eventID, videos := eventWithVideos(t, pool, 1)
	poster := posterMedia(t, pool, "event_cover", "image/png")
	writer, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Rollback(ctx)
	if _, err := writer.Exec(ctx, `UPDATE event_videos SET poster_media_id = $3 WHERE event_id = $1 AND media_id = $2`, eventID, videos[0], poster); err != nil {
		t.Fatal(err)
	}
	_, err = pool.Exec(ctx, `SELECT 1 FROM events WHERE id = $1 FOR NO KEY UPDATE NOWAIT`, eventID)
	var pgErr *pgconn.PgError
	if err != nil && !(errors.As(err, &pgErr) && pgErr.Code == "55P03") {
		t.Fatal(err)
	}
	return err != nil
}

// undoEventVideoPosters rolls back the poster migration, which builds on the
// Event files migration's link table, so that an older down migration can
// run; applying again brings it back. The video frames' migration, which
// builds on it, goes first.
func undoEventVideoPosters(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	undoEventVideoFrames(t, pool)
	down, err := fs.ReadFile(db.DownSQL, "migrations/"+eventVideoPostersVersion+"_event_video_posters.down.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(context.Background(), string(down)); err != nil {
		t.Fatalf("event video posters down: %v", err)
	}
	if _, err := pool.Exec(context.Background(), `DELETE FROM schema_migrations WHERE version = `+eventVideoPostersVersion); err != nil {
		t.Fatal(err)
	}
}

// withoutEventLock is the poster function as the migration writes it, but
// without the step that locks the Events.
func withoutEventLock(t *testing.T) string {
	t.Helper()
	up, err := fs.ReadFile(db.UpSQL, "migrations/"+eventVideoPostersVersion+"_event_video_posters.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	body := string(up)
	start := strings.Index(body, "CREATE OR REPLACE FUNCTION sync_event_video_poster_attachments()")
	end := strings.Index(body, "CREATE OR REPLACE TRIGGER event_videos_poster_attachments_insert")
	lock := regexp.MustCompile(`(?s)\n\s*PERFORM 1 FROM events.*?FOR NO KEY UPDATE;`)
	if start < 0 || end < start || !lock.MatchString(body[start:end]) {
		t.Fatal("the migration's poster function is not where the test looks for it")
	}
	return lock.ReplaceAllString(body[start:end], "")
}

// A database that lost part of the posters' schema, or whose role tables a
// rerun of the Event files migration put back (without the poster's role),
// is not taken for migrated: the migration runs again and puts it back.
func TestApplyRepairsTheVideoPosterLinks(t *testing.T) {
	eventFiles, err := fs.ReadFile(db.UpSQL, "migrations/"+eventFilesVersion+"_event_files.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	for name, damage := range map[string]string{
		"a lost trigger":        `DROP TRIGGER event_videos_poster_attachments_delete ON event_videos`,
		"the older role tables": string(eventFiles),
		"the gallery's function": `CREATE OR REPLACE TRIGGER event_videos_poster_attachments_update AFTER UPDATE ON event_videos
			REFERENCING OLD TABLE AS old_owners NEW TABLE AS new_owners
			FOR EACH STATEMENT EXECUTE FUNCTION sync_core_media_attachments('event', 'event_video_poster', 'event_id', 'poster_media_id')`,
		"a function that forgets a shared poster": `CREATE OR REPLACE FUNCTION sync_event_video_poster_attachments() RETURNS trigger
			LANGUAGE plpgsql AS $$ BEGIN RETURN NULL; END; $$`,
		"a function that does not lock the Event": withoutEventLock(t),
		"a lost foreign key":                      `ALTER TABLE event_videos DROP CONSTRAINT event_videos_poster_media_id_fkey`,
		"a foreign key that takes the video's row": `ALTER TABLE event_videos DROP CONSTRAINT event_videos_poster_media_id_fkey;
			ALTER TABLE event_videos ADD CONSTRAINT event_videos_poster_media_id_fkey FOREIGN KEY (poster_media_id) REFERENCES media (id) ON DELETE CASCADE`,
		"a lost index": `DROP INDEX event_videos_poster_media_id_idx`,
	} {
		t.Run(name, func(t *testing.T) {
			pool := postgresPool(t)
			ctx := context.Background()
			if err := migrate.Apply(ctx, pool); err != nil {
				t.Fatal(err)
			}
			if !postersHold(t, pool) || !sharedPosterKept(t, pool) || !posterChangeLocksTheEvent(t, pool) {
				t.Fatal("a fresh database does not keep videos' posters")
			}
			if _, err := pool.Exec(ctx, `DELETE FROM schema_migrations WHERE version = `+eventVideoPostersVersion); err != nil {
				t.Fatal(err)
			}
			if _, err := pool.Exec(ctx, damage); err != nil {
				t.Fatal(err)
			}
			if postersHold(t, pool) && sharedPosterKept(t, pool) && postersIndexed(t, pool) && posterChangeLocksTheEvent(t, pool) {
				t.Fatal("the damage broke nothing")
			}
			if err := migrate.Apply(ctx, pool); err != nil {
				t.Fatal(err)
			}
			if !postersHold(t, pool) || !sharedPosterKept(t, pool) || !postersIndexed(t, pool) || !posterChangeLocksTheEvent(t, pool) {
				t.Fatalf("%s was not repaired", name)
			}
		})
	}
}

// The down migration refuses while a video has a poster: dropping the
// column would leave the posters' Media attachments behind, and clearing
// them first would start their 30 days. Once none is left it drops the
// poster and puts back the role tables without its role; applying again
// brings it back.
func TestEventVideoPostersDownRefusesWhileAVideoHasAPoster(t *testing.T) {
	pool := postgresPool(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	down, err := fs.ReadFile(db.DownSQL, "migrations/"+eventVideoPostersVersion+"_event_video_posters.down.sql")
	if err != nil {
		t.Fatal(err)
	}
	eventID, _ := eventWithVideos(t, pool, 1)
	poster := posterMedia(t, pool, "event_gallery", "image/png")
	if _, err := pool.Exec(ctx, `UPDATE event_videos SET poster_media_id = $2 WHERE event_id = $1`, eventID, poster); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, string(down)); err == nil || !strings.Contains(err.Error(), "event video posters down") {
		t.Fatalf("down while a video has a poster: %v", err)
	}
	if !postersHold(t, pool) {
		t.Fatal("a refused down changed the schema")
	}

	if _, err := pool.Exec(ctx, `UPDATE event_videos SET poster_media_id = NULL WHERE event_id = $1`, eventID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, string(down)); err != nil {
		t.Fatalf("down once no video has a poster: %v", err)
	}
	var columns, functions int
	var posterRoleKnown, legacyFitsVideo bool
	if err := pool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM information_schema.columns WHERE table_name = 'event_videos' AND column_name = 'poster_media_id'),
		(SELECT count(*) FROM pg_proc WHERE proname = 'sync_event_video_poster_attachments'),
		EXISTS (SELECT 1 FROM media_role_purposes() WHERE role = 'event_video_poster'),
		media_purpose_fits_role('core', 'event_video', 'legacy')`, pgx.QueryExecModeSimpleProtocol).
		Scan(&columns, &functions, &posterRoleKnown, &legacyFitsVideo); err != nil || columns != 0 || functions != 0 || posterRoleKnown || legacyFitsVideo {
		t.Fatalf("after down: %d columns, %d functions, poster role known %v, legacy fits a video %v (err %v)", columns, functions, posterRoleKnown, legacyFitsVideo, err)
	}
	if !eventFilesRolesHold(t, pool) {
		t.Fatal("the posters' down took the Event files' roles with it")
	}
	if _, err := pool.Exec(ctx, `DELETE FROM schema_migrations WHERE version = `+eventVideoPostersVersion); err != nil {
		t.Fatal(err)
	}
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if !postersHold(t, pool) || !sharedPosterKept(t, pool) {
		t.Fatal("applying again after down did not bring the posters back")
	}
}
