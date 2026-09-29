package migrate_test

import (
	"context"
	"io/fs"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/skylab-kulubu/core-backend/db"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
)

const eventFilesVersion = "20260928160000"

// eventFilesRolesHold reports whether the database knows an Event's files
// and videos: the link tables' delete triggers are there, the role table
// names both roles, and a legacy Media fits neither.
func eventFilesRolesHold(t *testing.T, pool *pgxpool.Pool) bool {
	t.Helper()
	var triggers int
	var fileFits, videoFits, legacyFits bool
	// Planned afresh each time (the simple protocol): a prepared statement
	// would keep what these immutable functions answered when it was
	// planned, before a breakage or a repair replaced one they call.
	err := pool.QueryRow(context.Background(), `SELECT
		(SELECT count(*) FROM pg_trigger WHERE tgname IN ('event_files_media_attachments_delete', 'event_videos_media_attachments_delete')),
		media_purpose_fits_role('core', 'event_file', 'club_file'),
		media_purpose_fits_role('core', 'event_video', 'video'),
		media_purpose_fits_role('core', 'event_file', 'legacy') OR media_purpose_fits_role('core', 'event_video', 'legacy')`,
		pgx.QueryExecModeSimpleProtocol).
		Scan(&triggers, &fileFits, &videoFits, &legacyFits)
	if err != nil {
		t.Fatal(err)
	}
	return triggers == 2 && fileFits && videoFits && !legacyFits
}

// undoEventFiles rolls back the Event files migration, which builds on the
// Media attachment migration's link function, so that an older down
// migration can run; applying again brings it back. The video posters'
// migration, which builds on it, goes first.
func undoEventFiles(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	undoEventVideoPosters(t, pool)
	down, err := fs.ReadFile(db.DownSQL, "migrations/"+eventFilesVersion+"_event_files.down.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(context.Background(), string(down)); err != nil {
		t.Fatalf("event files down: %v", err)
	}
	if _, err := pool.Exec(context.Background(), `DELETE FROM schema_migrations WHERE version = `+eventFilesVersion); err != nil {
		t.Fatal(err)
	}
}

// A database that lost one of the Event file links' triggers, or whose role
// table and purpose check a rerun of the legacy backfill migration put back
// (without the two roles), is not taken for migrated: the migration runs
// again and puts them back.
func TestApplyRepairsTheEventFileLinks(t *testing.T) {
	pool := postgresPool(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if !eventFilesRolesHold(t, pool) {
		t.Fatal("a fresh database does not know an Event's files and videos")
	}
	backfill, err := fs.ReadFile(db.UpSQL, "migrations/20260926161000_media_legacy_backfill.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	for name, breakage := range map[string]string{
		"a lost trigger":           `DROP TRIGGER event_videos_media_attachments_delete ON event_videos`,
		"the older role table":     string(backfill),
		"a trigger without a role": `CREATE OR REPLACE TRIGGER event_files_media_attachments_delete AFTER DELETE ON event_files REFERENCING OLD TABLE AS old_owners FOR EACH STATEMENT EXECUTE FUNCTION sync_core_media_attachments('event', 'event_gallery', 'event_id', 'media_id')`,
		"a role legacy fits again": `CREATE OR REPLACE FUNCTION media_roles_without_legacy() RETURNS TABLE (owner_service TEXT, role TEXT)
			LANGUAGE sql IMMUTABLE AS $$ VALUES ('core', 'event_file') $$`,
		"a trigger reading other transition tables": `CREATE OR REPLACE TRIGGER event_videos_media_attachments_update AFTER UPDATE ON event_videos
			REFERENCING OLD TABLE AS before_owners NEW TABLE AS new_owners
			FOR EACH STATEMENT EXECUTE FUNCTION sync_core_media_attachments('event', 'event_video', 'event_id', 'media_id')`,
	} {
		if _, err := pool.Exec(ctx, breakage); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if (name == "a lost trigger" || name == "the older role table" || name == "a role legacy fits again") && eventFilesRolesHold(t, pool) {
			t.Fatalf("%s: the breakage broke nothing", name)
		}
		if _, err := pool.Exec(ctx, `DROP TABLE schema_migrations`); err != nil {
			t.Fatal(err)
		}
		if err := migrate.Apply(ctx, pool); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !eventFilesRolesHold(t, pool) {
			t.Fatalf("%s was not repaired", name)
		}
		var role, oldTable string
		if err := pool.QueryRow(ctx, `SELECT encode(tgargs, 'escape') FROM pg_trigger WHERE tgname = 'event_files_media_attachments_delete'`).Scan(&role); err != nil || !strings.Contains(role, "event_file\\000") {
			t.Fatalf("%s: the file delete trigger's arguments are %q (err %v)", name, role, err)
		}
		if err := pool.QueryRow(ctx, `SELECT tgoldtable FROM pg_trigger WHERE tgname = 'event_videos_media_attachments_update'`).Scan(&oldTable); err != nil || oldTable != "old_owners" {
			t.Fatalf("%s: the video update trigger reads the old links as %q (err %v)", name, oldTable, err)
		}
	}
}

// The down migration refuses while an Event holds a file or a video:
// dropping the link tables would leave their Media attachments behind, and
// removing the links first would start the files' 30 days.
func TestEventFilesDownRefusesWhileAnEventHoldsAFile(t *testing.T) {
	pool := postgresPool(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	down, err := fs.ReadFile(db.DownSQL, "migrations/"+eventFilesVersion+"_event_files.down.sql")
	if err != nil {
		t.Fatal(err)
	}
	file, err := scanMedia(t, pool, "attached", nil)
	if err != nil {
		t.Fatal(err)
	}
	eventID := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO events (id, name, location, owner_team) VALUES ($1, 'Hack', 'YTÜ', 'WEBLAB')`, eventID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO event_files (event_id, media_id, order_index) VALUES ($1, $2, 1)`, eventID, file); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, string(down)); err == nil || !strings.Contains(err.Error(), "event files down") {
		t.Fatalf("down while an Event holds a file: %v", err)
	}
	if !eventFilesRolesHold(t, pool) {
		t.Fatal("a refused down changed the schema")
	}

	if _, err := pool.Exec(ctx, `DELETE FROM event_files WHERE event_id = $1`, eventID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, string(down)); err != nil {
		t.Fatalf("down once no Event holds a file: %v", err)
	}
	var tables int
	var legacyFits bool
	if err := pool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM pg_tables WHERE tablename IN ('event_files', 'event_videos')),
		media_purpose_fits_role('core', 'event_file', 'legacy')`).Scan(&tables, &legacyFits); err != nil || tables != 0 || !legacyFits {
		t.Fatalf("after down: %d link tables, legacy fits %v (err %v)", tables, legacyFits, err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM schema_migrations WHERE version = `+eventFilesVersion); err != nil {
		t.Fatal(err)
	}
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if !eventFilesRolesHold(t, pool) {
		t.Fatal("applying again after down did not bring the Event files back")
	}
}
