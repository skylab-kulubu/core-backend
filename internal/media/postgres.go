package media

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/skylab-kulubu/core-backend/internal/lifecycle"
	"github.com/skylab-kulubu/core-backend/internal/subjectlock"
)

type PostgresStore struct {
	pool *pgxpool.Pool
}

func NewPostgresStore(pool *pgxpool.Pool) *PostgresStore {
	return &PostgresStore{pool: pool}
}

const mediaCols = `id, file_name, file_type, file_url, file_size, uploaded_by, kind, cover_colors, cover_colors_computed, deleted_at, deleted_by, blob_purge_started_at, blob_purged_at, blob_purge_checked_at, created_at, updated_at, serving_policy_applied, purpose, status, expires_at`

func (s *PostgresStore) Create(ctx context.Context, m Media) (Media, error) {
	return insertMedia(ctx, s.pool, newRecord(m))
}

type rowQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// insertMedia writes a record prepared by newRecord.
func insertMedia(ctx context.Context, db rowQuerier, m Media) (Media, error) {
	created, err := scanMedia(db.QueryRow(ctx, `
		INSERT INTO media (id, file_name, file_type, file_url, file_size, uploaded_by, kind, cover_colors, cover_colors_computed, serving_policy_applied, purpose, status, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
		RETURNING `+mediaCols, m.ID, m.Name, m.Type, m.Key, m.Size, m.UploadedBy, m.Kind, m.CoverColors, m.CoverColorsComputed, m.ServingPolicyApplied, m.Purpose, m.Status, m.ExpiresAt))
	if subjectlock.IsInactiveAccountReference(err) {
		return Media{}, ErrForbidden
	}
	return created, err
}

func (s *PostgresStore) Get(ctx context.Context, id uuid.UUID) (Media, error) {
	return s.get(ctx, id, false)
}

func (s *PostgresStore) GetIncludingDeleted(ctx context.Context, id uuid.UUID) (Media, error) {
	return s.get(ctx, id, true)
}

func (s *PostgresStore) get(ctx context.Context, id uuid.UUID, includeDeleted bool) (Media, error) {
	q := `SELECT ` + mediaCols + ` FROM media WHERE id = $1`
	if !includeDeleted {
		q += ` AND deleted_at IS NULL`
	}
	m, err := scanMedia(s.pool.QueryRow(ctx, q, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Media{}, ErrNotFound
	}
	return m, err
}

func (s *PostgresStore) List(ctx context.Context) ([]Media, error) {
	return s.list(ctx, lifecycle.CurrentOnly)
}

func (s *PostgresStore) ListLifecycle(ctx context.Context, visibility lifecycle.Visibility) ([]Media, error) {
	return s.list(ctx, visibility)
}

func (s *PostgresStore) list(ctx context.Context, visibility lifecycle.Visibility) ([]Media, error) {
	q := `SELECT ` + mediaCols + ` FROM media`
	if condition := visibility.SQLCondition("deleted_at"); condition != "" {
		q += ` WHERE ` + condition
	}
	rows, err := s.pool.Query(ctx, q+` ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Media, 0)
	for rows.Next() {
		m, err := scanMedia(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *PostgresStore) ListPendingCoverColors(ctx context.Context, limit int) ([]Media, error) {
	if limit <= 0 {
		limit = 25
	}
	rows, err := s.pool.Query(ctx, `SELECT `+mediaCols+` FROM media WHERE kind = $1 AND cover_colors_computed = false AND deleted_at IS NULL ORDER BY created_at LIMIT $2`, KindImage, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Media, 0)
	for rows.Next() {
		m, err := scanMedia(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *PostgresStore) SetCoverColors(ctx context.Context, id uuid.UUID, colors []string) error {
	tag, err := s.pool.Exec(ctx, `UPDATE media SET cover_colors = $2, cover_colors_computed = true, updated_at = now() WHERE id = $1 AND deleted_at IS NULL`, id, colors)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *PostgresStore) ListPendingServingPolicy(ctx context.Context, after uuid.UUID, limit int) ([]Media, error) {
	if limit <= 0 {
		limit = 25
	}
	rows, err := s.pool.Query(ctx, `SELECT `+mediaCols+` FROM media
		WHERE serving_policy_applied = false AND id > $1 AND blob_purge_started_at IS NULL AND blob_purged_at IS NULL
		ORDER BY id LIMIT $2`, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Media, 0)
	for rows.Next() {
		m, err := scanMedia(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *PostgresStore) SetServingPolicyApplied(ctx context.Context, id uuid.UUID) error {
	tag, err := s.pool.Exec(ctx, `UPDATE media SET serving_policy_applied = true WHERE id = $1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *PostgresStore) Archive(ctx context.Context, id uuid.UUID, actorID *uuid.UUID) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE media
		SET deleted_at = COALESCE(deleted_at, now()),
			deleted_by = CASE WHEN deleted_at IS NULL THEN $2 ELSE deleted_by END,
			updated_at = CASE WHEN deleted_at IS NULL THEN now() ELSE updated_at END
		WHERE id = $1`, id, actorID)
	if err != nil {
		if subjectlock.IsInactiveAccountReference(err) {
			return ErrForbidden
		}
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *PostgresStore) Restore(ctx context.Context, id uuid.UUID) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE media
		SET deleted_at = NULL,
			deleted_by = NULL,
			expires_at = CASE WHEN deleted_at IS NOT NULL THEN NULL ELSE expires_at END,
			updated_at = CASE WHEN deleted_at IS NOT NULL THEN now() ELSE updated_at END
		WHERE id = $1 AND blob_purge_started_at IS NULL AND blob_purged_at IS NULL`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		m, getErr := s.GetIncludingDeleted(ctx, id)
		if getErr != nil {
			return getErr
		}
		if m.BlobPurgedAt != nil {
			return ErrPurged
		}
		if m.BlobPurgeStartedAt != nil {
			return ErrPurgeInProgress
		}
		return ErrNotFound
	}
	return nil
}

func (s *PostgresStore) ExpireUnattachedAt(ctx context.Context, id uuid.UUID, at *time.Time) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE media SET expires_at = $2
		WHERE id = $1 AND status <> 'attached' AND deleted_at IS NULL
		  AND blob_purge_started_at IS NULL AND blob_purged_at IS NULL`, id, at)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		if _, err := s.Get(ctx, id); err != nil {
			return err
		}
	}
	return nil
}

func (s *PostgresStore) ListPurgeCandidates(ctx context.Context, deletedBefore time.Time, limit int) ([]Media, error) {
	if limit <= 0 {
		limit = 25
	}
	rows, err := s.pool.Query(ctx, `SELECT `+mediaCols+` FROM media
		WHERE deleted_at IS NOT NULL AND deleted_at <= $1 AND blob_purged_at IS NULL
		ORDER BY blob_purge_started_at NULLS LAST, COALESCE(blob_purge_checked_at, deleted_at), id LIMIT $2`, deletedBefore, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Media, 0)
	for rows.Next() {
		m, err := scanMedia(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// ListExpired returns, in id order and after the given id, the Media the
// expiry cleanup purges at now: pending or detached, expired, with a blob.
// Archived Media are left to the archive window, unless the cleanup already
// claimed their purge.
func (s *PostgresStore) ListExpired(ctx context.Context, now time.Time, after uuid.UUID, limit int) ([]Media, error) {
	if limit <= 0 {
		limit = 25
	}
	rows, err := s.pool.Query(ctx, `SELECT `+mediaCols+` FROM media
		WHERE status IN ('pending', 'detached') AND expires_at <= $1 AND blob_purged_at IS NULL
		  AND (deleted_at IS NULL OR blob_purge_started_at IS NOT NULL)
		  AND id > $2
		ORDER BY id LIMIT $3`, now, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Media, 0)
	for rows.Next() {
		m, err := scanMedia(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// The Media a purge may start on, as a condition on the media row;
// purge.at is the purge's time. A purge already started goes on whatever the
// condition says.
const (
	// archivedPurge: an archived Media. The caller decides the window.
	archivedPurge = `media.deleted_at IS NOT NULL`
	// expiredPurge: a current Media no attachment keeps, past its expiry.
	expiredPurge = `media.deleted_at IS NULL AND media.status IN ('pending', 'detached') AND media.expires_at <= purge.at`
)

func (s *PostgresStore) PurgeBlobIfUnreferenced(ctx context.Context, id uuid.UUID, purgedAt time.Time, purge func(string) error) (bool, error) {
	return s.purgeBlob(ctx, id, purgedAt, archivedPurge, purge)
}

// PurgeExpiredBlobIfUnreferenced purges the blob of a Media past its expiry
// the way PurgeBlobIfUnreferenced purges an archived one, and archives the
// Media as its blob goes.
func (s *PostgresStore) PurgeExpiredBlobIfUnreferenced(ctx context.Context, id uuid.UUID, now time.Time, purge func(string) error) (bool, error) {
	return s.purgeBlob(ctx, id, now, expiredPurge, purge)
}

// purgeBlob is the two-phase purge: a durable claim after the locked
// reference check, the idempotent object deletion, then the record.
func (s *PostgresStore) purgeBlob(ctx context.Context, id uuid.UUID, purgedAt time.Time, eligible string, purge func(string) error) (bool, error) {
	claimed, err := s.claimBlobPurge(ctx, id, purgedAt, eligible)
	if err != nil || !claimed {
		return false, err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)

	if err := lockMediaReferenceWriters(ctx, tx); err != nil {
		return false, err
	}
	var key string
	err = tx.QueryRow(ctx, `SELECT file_url FROM media
		WHERE id = $1 AND blob_purge_started_at IS NOT NULL AND blob_purged_at IS NULL
		FOR UPDATE`, id).Scan(&key)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}

	var referenced bool
	referenced, err = mediaReferenced(ctx, tx, id)
	if err != nil {
		return false, err
	}
	if referenced {
		if _, err := tx.Exec(ctx, `UPDATE media SET blob_purge_started_at = NULL, blob_purge_checked_at = $2 WHERE id = $1`, id, purgedAt); err != nil {
			return false, err
		}
		if err := tx.Commit(ctx); err != nil {
			return false, err
		}
		return false, nil
	}
	if err := purge(key); err != nil {
		return false, err
	}
	if _, err := tx.Exec(ctx, `UPDATE media
		SET blob_purged_at = $2, blob_purge_checked_at = $2, updated_at = $2, deleted_at = COALESCE(deleted_at, $2)
		WHERE id = $1`, id, purgedAt); err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return true, nil
}

type postgresMediaTx interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

func lockMediaReferenceWriters(ctx context.Context, tx postgresMediaTx) error {
	_, err := tx.Exec(ctx, `LOCK TABLE events, event_images, users, certificate_templates, certificate_template_versions, media_attachments IN SHARE MODE`)
	return err
}

// mediaReferenced reports whether anything still uses the Media: a Media
// attachment, or one of core's own links. The links are checked directly
// too, as a safety net, until the legacy backfill (media redesign ticket 08)
// has proven every link has its attachment: a Media is unreferenced only when
// both say so.
func mediaReferenced(ctx context.Context, tx postgresMediaTx, id uuid.UUID) (bool, error) {
	var referenced bool
	err := tx.QueryRow(ctx, `SELECT EXISTS (
		SELECT 1 FROM media_attachments WHERE media_id = $1
		UNION ALL SELECT 1 FROM events WHERE cover_image_id = $1
		UNION ALL SELECT 1 FROM event_images WHERE media_id = $1
		UNION ALL SELECT 1 FROM users WHERE profile_picture_id = $1
		UNION ALL SELECT 1 FROM certificate_templates
			WHERE draft_layout->>'backgroundMediaId' = $1::text
			   OR EXISTS (SELECT 1 FROM jsonb_array_elements(COALESCE(draft_layout->'elements', '[]'::jsonb)) element WHERE element->>'mediaId' = $1::text)
		UNION ALL SELECT 1 FROM certificate_template_versions
			WHERE layout->>'backgroundMediaId' = $1::text
			   OR asset_manifest ? $1::text
			   OR EXISTS (SELECT 1 FROM jsonb_array_elements(COALESCE(layout->'elements', '[]'::jsonb)) element WHERE element->>'mediaId' = $1::text)
	)`, id).Scan(&referenced)
	return referenced, err
}

func (s *PostgresStore) claimBlobPurge(ctx context.Context, id uuid.UUID, claimedAt time.Time, eligible string) (bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	if err := lockMediaReferenceWriters(ctx, tx); err != nil {
		return false, err
	}
	var startedAt, purgedAt *time.Time
	var claimable bool
	err = tx.QueryRow(ctx, `SELECT media.blob_purge_started_at, media.blob_purged_at, `+eligible+`
		FROM media, (SELECT $2::timestamptz AS at) purge
		WHERE media.id = $1
		FOR UPDATE OF media`, id, claimedAt).Scan(&startedAt, &purgedAt, &claimable)
	if errors.Is(err, pgx.ErrNoRows) || purgedAt != nil {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if startedAt == nil {
		if !claimable {
			return false, nil
		}
		referenced, err := mediaReferenced(ctx, tx, id)
		if err != nil {
			return false, err
		}
		if referenced {
			if _, err := tx.Exec(ctx, `UPDATE media SET blob_purge_checked_at = $2 WHERE id = $1`, id, claimedAt); err != nil {
				return false, err
			}
			if err := tx.Commit(ctx); err != nil {
				return false, err
			}
			return false, nil
		}
		if _, err := tx.Exec(ctx, `UPDATE media SET blob_purge_started_at = $2, blob_purge_checked_at = $2 WHERE id = $1`, id, claimedAt); err != nil {
			return false, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return true, nil
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanMedia(row rowScanner) (Media, error) {
	var m Media
	var created, updated time.Time
	err := row.Scan(&m.ID, &m.Name, &m.Type, &m.Key, &m.Size, &m.UploadedBy, &m.Kind, &m.CoverColors, &m.CoverColorsComputed, &m.DeletedAt, &m.DeletedBy, &m.BlobPurgeStartedAt, &m.BlobPurgedAt, &m.BlobPurgeCheckedAt, &created, &updated, &m.ServingPolicyApplied, &m.Purpose, &m.Status, &m.ExpiresAt)
	if m.CoverColors == nil {
		m.CoverColors = []string{}
	}
	m.CreatedAt = created
	m.UpdatedAt = updated
	return m, err
}
