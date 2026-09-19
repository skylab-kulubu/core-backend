package migrate

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"path"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/skylab-kulubu/core-backend/db"
)

type file struct {
	version int64
	name    string
}

var fingerprints = map[int64]string{
	20260916140000: `SELECT 1 FROM information_schema.tables WHERE table_schema = 'public' AND table_name = 'users'`,
	20260916160000: `SELECT 1 FROM information_schema.tables WHERE table_schema = 'public' AND table_name = 'events'`,
	20260916170000: `SELECT 1 FROM information_schema.tables WHERE table_schema = 'public' AND table_name = 'tickets'`,
	20260916180000: `SELECT 1 FROM information_schema.tables WHERE table_schema = 'public' AND table_name = 'sessions'`,
	20260916190000: `SELECT 1 FROM information_schema.tables WHERE table_schema = 'public' AND table_name = 'competitors'`,
	20260916200000: `SELECT 1 FROM information_schema.tables WHERE table_schema = 'public' AND table_name = 'media'`,
	20260917100000: `SELECT 1 FROM information_schema.tables WHERE table_schema = 'public' AND table_name = 'urls'`,
	20260917110000: `SELECT 1 FROM information_schema.columns WHERE table_schema = 'public' AND table_name = 'events' AND column_name = 'cover_image_id'`,
	20260917120000: `SELECT 1 FROM information_schema.columns WHERE table_schema = 'public' AND table_name = 'users' AND column_name = 'school_email'`,
	20260917130000: `SELECT 1 FROM information_schema.columns WHERE table_schema = 'public' AND table_name = 'users' AND column_name = 'sky_number'`,
	20260917140000: `SELECT 1 FROM information_schema.columns WHERE table_schema = 'public' AND table_name = 'users' AND column_name = 'username'`,
	20260917150000: `SELECT 1 FROM information_schema.tables WHERE table_schema = 'public' AND table_name = 'event_images'`,
	20260917160000: `SELECT 1 FROM information_schema.columns WHERE table_schema = 'public' AND table_name = 'users' AND column_name = 'student_card_uid'`,
	20260917170000: `SELECT 1 FROM information_schema.columns WHERE table_schema = 'public' AND table_name = 'ticket_checkins' AND column_name = 'session_id'`,
	20260917171000: `SELECT 1 FROM information_schema.tables WHERE table_schema = 'public' AND table_name = 'event_door_staff'`,
	20260917180000: `SELECT 1 FROM information_schema.tables WHERE table_schema = 'public' AND table_name = 'certificates'`,
	20260918180000: `SELECT 1 FROM information_schema.columns WHERE table_schema = 'public' AND table_name = 'events' AND column_name = 'extra_form_urls'`,
	20260919120000: `SELECT 1 FROM information_schema.columns WHERE table_schema = 'public' AND table_name = 'users' AND column_name = 'phone'`,
	20260919130000: `SELECT 1 FROM information_schema.tables WHERE table_schema = 'public' AND table_name = 'url_hits' AND to_regclass('public.url_hits_url_id_at_idx') IS NOT NULL`,
	20260919200000: `SELECT 1 FROM information_schema.tables WHERE table_schema = 'public' AND table_name = 'certificate_templates'`,
	20260919210000: `
		SELECT 1
		WHERE (
			SELECT count(*)
			FROM (VALUES
				('events', 'archived_at'),
				('events', 'archived_by'),
				('event_days', 'archived_at'),
				('event_days', 'archived_by'),
				('sessions', 'archived_at'),
				('sessions', 'archived_by'),
				('seasons', 'archived_at'),
				('seasons', 'archived_by'),
				('competitors', 'withdrawn_at'),
				('competitors', 'withdrawn_by'),
				('media', 'deleted_at'),
				('media', 'deleted_by'),
				('urls', 'disabled_at'),
				('urls', 'disabled_by')
			) AS expected(table_name, column_name)
			JOIN information_schema.columns actual
			  ON actual.table_schema = 'public'
			 AND actual.table_name = expected.table_name
			 AND actual.column_name = expected.column_name
		) = 14
		AND (
			SELECT count(*)
			FROM (VALUES
				('events_current_owner_team_idx'),
				('event_days_current_event_idx'),
				('sessions_current_event_day_idx'),
				('seasons_current_start_date_idx'),
				('competitors_current_event_idx'),
				('competitors_current_user_idx'),
				('media_current_created_at_idx'),
				('urls_current_created_by_idx')
			) AS expected(index_name)
			JOIN pg_indexes actual
			  ON actual.schemaname = 'public'
			 AND actual.indexname = expected.index_name
		) = 8`,
	20260919211000: `
		SELECT 1
		WHERE (
			SELECT count(*)
			FROM (VALUES
				('blob_purge_started_at'),
				('blob_purged_at'),
				('blob_purge_checked_at')
			) AS expected(column_name)
			JOIN information_schema.columns actual
			  ON actual.table_schema = 'public'
			 AND actual.table_name = 'media'
			 AND actual.column_name = expected.column_name
		) = 3
		AND to_regclass('public.media_blob_purge_candidates_idx') IS NOT NULL
		AND (
			SELECT count(DISTINCT actual.trigger_name)
			FROM (VALUES
				('events_require_current_cover_media'),
				('event_images_require_current_media'),
				('users_require_current_profile_media'),
				('certificate_templates_require_current_media'),
				('certificate_template_versions_require_current_media')
			) AS expected(trigger_name)
			JOIN information_schema.triggers actual
			  ON actual.trigger_schema = 'public'
			 AND actual.trigger_name = expected.trigger_name
		) = 5`,
}

func Apply(ctx context.Context, pool *pgxpool.Pool) error {
	if _, err := pool.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version BIGINT PRIMARY KEY,
			applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`); err != nil {
		return fmt.Errorf("schema_migrations: %w", err)
	}

	files, err := listUp()
	if err != nil {
		return err
	}
	applied, err := loadApplied(ctx, pool)
	if err != nil {
		return err
	}

	for _, f := range files {
		if applied[f.version] {
			continue
		}
		present, err := alreadyPresent(ctx, pool, f.version)
		if err != nil {
			return fmt.Errorf("fingerprint %d: %w", f.version, err)
		}
		if present {
			if err := record(ctx, pool, f.version); err != nil {
				return err
			}
			log.Printf("migrate: recorded existing %d", f.version)
			continue
		}
		body, err := fs.ReadFile(db.UpSQL, path.Join("migrations", f.name))
		if err != nil {
			return err
		}
		if err := execSQL(ctx, pool, string(body)); err != nil {
			return fmt.Errorf("migration %d: %w", f.version, err)
		}
		if err := record(ctx, pool, f.version); err != nil {
			return err
		}
		log.Printf("migrate: applied %d", f.version)
	}
	return nil
}

func Versions() ([]int64, error) {
	files, err := listUp()
	if err != nil {
		return nil, err
	}
	out := make([]int64, len(files))
	for i, f := range files {
		out[i] = f.version
	}
	return out, nil
}

func ExtraFormSQL() (string, error) {
	body, err := fs.ReadFile(db.UpSQL, "migrations/20260918180000_event_extra_form_urls.up.sql")
	if err != nil {
		return "", err
	}
	return string(body), nil
}

func listUp() ([]file, error) {
	entries, err := fs.ReadDir(db.UpSQL, "migrations")
	if err != nil {
		return nil, err
	}
	out := make([]file, 0, len(entries))
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".up.sql") {
			continue
		}
		v, err := strconv.ParseInt(strings.TrimSuffix(name, path.Ext(name))[:14], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("migration name %s: %w", name, err)
		}
		out = append(out, file{version: v, name: name})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].version < out[j].version })
	return out, nil
}

func loadApplied(ctx context.Context, pool *pgxpool.Pool) (map[int64]bool, error) {
	rows, err := pool.Query(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]bool{}
	for rows.Next() {
		var v int64
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out[v] = true
	}
	return out, rows.Err()
}

func alreadyPresent(ctx context.Context, pool *pgxpool.Pool, version int64) (bool, error) {
	q, ok := fingerprints[version]
	if !ok {
		return false, nil
	}
	var n int
	err := pool.QueryRow(ctx, q).Scan(&n)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

func record(ctx context.Context, pool *pgxpool.Pool, version int64) error {
	_, err := pool.Exec(ctx, `INSERT INTO schema_migrations (version) VALUES ($1) ON CONFLICT DO NOTHING`, version)
	return err
}

func execSQL(ctx context.Context, pool *pgxpool.Pool, sql string) error {
	sql = strings.TrimSpace(sql)
	if sql == "" {
		return nil
	}
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	_, err = conn.Conn().PgConn().Exec(ctx, sql).ReadAll()
	return err
}
