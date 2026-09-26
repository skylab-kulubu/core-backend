package media

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
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

const mediaCols = `id, file_name, file_type, file_url, file_size, uploaded_by, kind, cover_colors, cover_colors_computed, deleted_at, deleted_by, blob_purge_started_at, blob_purged_at, blob_purge_checked_at, created_at, updated_at, serving_policy_applied, purpose, status, expires_at, detach_expiry_held, width, height, size_objects`

func (s *PostgresStore) Create(ctx context.Context, m Media) (Media, error) {
	return insertMedia(ctx, s.pool, newRecord(m))
}

type rowQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// insertMedia writes a record prepared by newRecord.
func insertMedia(ctx context.Context, db rowQuerier, m Media) (Media, error) {
	sizeObjects, err := sizeObjectsColumn(m.SizeObjects)
	if err != nil {
		return Media{}, err
	}
	created, err := scanMedia(db.QueryRow(ctx, `
		INSERT INTO media (id, file_name, file_type, file_url, file_size, uploaded_by, kind, cover_colors, cover_colors_computed, serving_policy_applied, purpose, status, expires_at, width, height, size_objects)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16::jsonb)
		RETURNING `+mediaCols, m.ID, m.Name, m.Type, m.Key, m.Size, m.UploadedBy, m.Kind, m.CoverColors, m.CoverColorsComputed, m.ServingPolicyApplied, m.Purpose, m.Status, m.ExpiresAt,
		positiveOrNil(m.Width), positiveOrNil(m.Height), sizeObjects))
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

// currentSQL holds for a current Media: not archived, no purge started.
const currentSQL = `media.deleted_at IS NULL AND media.blob_purge_started_at IS NULL AND media.blob_purged_at IS NULL`

// unattachedCurrentSQL holds for a current Media no Media attachment keeps.
const unattachedCurrentSQL = `media.status <> 'attached' AND ` + currentSQL

// ExpireUnattachedAt sets the expiry; a Media whose detach expiry is held
// (the legacy backfill, decision K2) keeps none, as a legacy one does.
func (s *PostgresStore) ExpireUnattachedAt(ctx context.Context, id uuid.UUID, at *time.Time) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE media SET expires_at = CASE WHEN media.detach_expiry_held THEN NULL ELSE $2::TIMESTAMPTZ END
		WHERE id = $1 AND `+unattachedCurrentSQL, id, at)
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

// expiredSQL is the database's counterpart of Media.expired, with the time
// as $1. It is NULL for a Media with no expiry: a WHERE clause leaves such a
// Media out, and a value read from it goes through COALESCE.
const expiredSQL = `expires_at <= $1`

// ListExpired returns, in id order and after the given id, the Media the
// expiry cleanup purges at now: expired, with a blob. Archived Media are left
// to the archive window, unless the cleanup already claimed their purge.
func (s *PostgresStore) ListExpired(ctx context.Context, now time.Time, after uuid.UUID, limit int) ([]Media, error) {
	if limit <= 0 {
		limit = 25
	}
	rows, err := s.pool.Query(ctx, `SELECT `+mediaCols+` FROM media
		WHERE `+expiredSQL+` AND blob_purged_at IS NULL
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

// purgeQueue is why a Media's blob may be purged.
type purgeQueue int

const (
	// archivedQueue: the Media is archived. The caller decides the window.
	archivedQueue purgeQueue = iota
	// expiredQueue: the Media is current and past its expiry.
	expiredQueue
)

// claimable reports whether a purge that has not started yet may start on a
// Media in this state.
func (q purgeQueue) claimable(archived, expired bool) bool {
	if q == expiredQueue {
		return !archived && expired
	}
	return archived
}

func (s *PostgresStore) PurgeBlobIfUnreferenced(ctx context.Context, id uuid.UUID, purgedAt time.Time, purge func(string) error) (bool, error) {
	return s.purgeBlob(ctx, id, purgedAt, archivedQueue, purge)
}

// PurgeExpiredBlobIfUnattached purges the blob of a Media past its expiry
// the way PurgeBlobIfUnreferenced purges an archived one, and archives the
// Media as its blob goes.
func (s *PostgresStore) PurgeExpiredBlobIfUnattached(ctx context.Context, id uuid.UUID, now time.Time, purge func(string) error) (bool, error) {
	return s.purgeBlob(ctx, id, now, expiredQueue, purge)
}

// purgeBlob is the two-phase purge: a durable claim after the locked
// reference check, the idempotent object deletion, then the record.
func (s *PostgresStore) purgeBlob(ctx context.Context, id uuid.UUID, purgedAt time.Time, queue purgeQueue, purge func(string) error) (bool, error) {
	claimed, err := s.claimBlobPurge(ctx, id, purgedAt, queue)
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
	if err := purgeObjects(key, purge); err != nil {
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
// has proven every link has its Media attachment: a Media is unused only when
// both say so.
func mediaReferenced(ctx context.Context, tx postgresMediaTx, id uuid.UUID) (bool, error) {
	var referenced bool
	err := tx.QueryRow(ctx, `SELECT EXISTS (
		SELECT 1 FROM media_attachments WHERE media_id = $1
		UNION ALL `+coreLinksSQL("$1")+`
	)`, id).Scan(&referenced)
	return referenced, err
}

// coreLinkSources are core's own links as their records hold them: the
// hard-coded list the purge and the legacy report read beside the Media
// attachments, as a safety net. Migration 20260926120000 wrote the first
// Media attachments from the same list and its triggers keep them in step
// (its copy stays as written: migrations are frozen).
var coreLinkSources = []struct {
	table     string
	ownerType string
	role      Role
	owner     string
	// media names the column that holds the Media id, or the certificate
	// layout with it; manifest a published version's asset manifest.
	media, manifest string
	layout          bool
}{
	{table: "events", ownerType: "event", role: RoleEventCover, owner: "id", media: "cover_image_id"},
	{table: "event_images", ownerType: "event", role: RoleEventGallery, owner: "event_id", media: "media_id"},
	{table: "users", ownerType: "user", role: RoleProfilePicture, owner: "id", media: "profile_picture_id"},
	{table: "certificate_templates", ownerType: "certificate_template", role: RoleCertificateAsset, owner: "id", media: "draft_layout", layout: true},
	{table: "certificate_template_versions", ownerType: "certificate_template_version", role: RoleCertificateAsset, owner: "id", media: "layout", manifest: "asset_manifest", layout: true},
}

// coreLinksSQL selects a row for each of core's own links to the Media id
// names (a parameter or a column).
func coreLinksSQL(id string) string {
	selects := make([]string, 0, len(coreLinkSources))
	for _, source := range coreLinkSources {
		match := source.media + ` = ` + id
		if source.layout {
			manifest := "NULL"
			if source.manifest != "" {
				manifest = source.manifest
			}
			match = id + ` = ANY (certificate_layout_media_ids(` + source.media + `, ` + manifest + `))`
		}
		selects = append(selects, `SELECT 1 FROM `+source.table+` WHERE `+match)
	}
	return strings.Join(selects, "\n\t\tUNION ALL ")
}

// coreLinkRowsSQL selects each of core's own links as (owner_type, role,
// owner_id, media_id), read as the Media attachment migration read them.
func coreLinkRowsSQL() string {
	selects := make([]string, 0, len(coreLinkSources))
	for _, source := range coreLinkSources {
		manifest := "NULL::TEXT"
		if source.manifest != "" {
			manifest = `'` + source.manifest + `'`
		}
		selects = append(selects, `SELECT '`+source.ownerType+`', '`+string(source.role)+`', to_jsonb(owner), '`+source.owner+`', '`+source.media+`', `+manifest+` FROM `+source.table+` owner`)
	}
	return `SELECT source.owner_type, source.role, link.owner_id, link.media_id
		FROM (` + strings.Join(selects, "\n\t\tUNION ALL ") + `) source (owner_type, role, record, owner_column, media_column, manifest_column)
		CROSS JOIN LATERAL core_media_links(source.record, source.owner_column, source.media_column, source.manifest_column) link`
}

func (s *PostgresStore) claimBlobPurge(ctx context.Context, id uuid.UUID, claimedAt time.Time, queue purgeQueue) (bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	if err := lockMediaReferenceWriters(ctx, tx); err != nil {
		return false, err
	}
	var startedAt, purgedAt *time.Time
	var archived, expired bool
	err = tx.QueryRow(ctx, `SELECT blob_purge_started_at, blob_purged_at, deleted_at IS NOT NULL, COALESCE(`+expiredSQL+`, false)
		FROM media WHERE id = $2 FOR UPDATE`, claimedAt, id).Scan(&startedAt, &purgedAt, &archived, &expired)
	if errors.Is(err, pgx.ErrNoRows) || purgedAt != nil {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if startedAt == nil {
		if !queue.claimable(archived, expired) {
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
	var width, height *int
	var sizeObjects []byte
	err := row.Scan(&m.ID, &m.Name, &m.Type, &m.Key, &m.Size, &m.UploadedBy, &m.Kind, &m.CoverColors, &m.CoverColorsComputed, &m.DeletedAt, &m.DeletedBy, &m.BlobPurgeStartedAt, &m.BlobPurgedAt, &m.BlobPurgeCheckedAt, &created, &updated, &m.ServingPolicyApplied, &m.Purpose, &m.Status, &m.ExpiresAt, &m.DetachExpiryHeld, &width, &height, &sizeObjects)
	if err != nil {
		return m, err
	}
	if m.CoverColors == nil {
		m.CoverColors = []string{}
	}
	if width != nil && height != nil {
		m.Width, m.Height = *width, *height
	}
	if sizeObjects != nil {
		if err := json.Unmarshal(sizeObjects, &m.SizeObjects); err != nil {
			return m, fmt.Errorf("media %s size objects: %w", m.ID, err)
		}
		if m.SizeObjects == nil {
			m.SizeObjects = map[string]SizeObject{}
		}
	}
	m.CreatedAt = created
	m.UpdatedAt = updated
	return m, nil
}

// sizeObjectsColumn is how SizeObjects is written: NULL while the sizes are
// not made, a JSON object once they are.
func sizeObjectsColumn(objects map[string]SizeObject) (*string, error) {
	if objects == nil {
		return nil, nil
	}
	encoded, err := json.Marshal(objects)
	if err != nil {
		return nil, err
	}
	column := string(encoded)
	return &column, nil
}

func positiveOrNil(n int) *int {
	if n <= 0 {
		return nil
	}
	return &n
}

func (s *PostgresStore) ListPendingImageSizes(ctx context.Context, purposes []string, after uuid.UUID, limit int) ([]Media, error) {
	if limit <= 0 {
		limit = 25
	}
	// The conditions repeat media_size_objects_pending_idx's predicate
	// literally, so the planner can use the partial index.
	rows, err := s.pool.Query(ctx, `SELECT `+mediaCols+` FROM media
		WHERE size_objects IS NULL AND kind = 'IMAGE' AND deleted_at IS NULL
		  AND blob_purge_started_at IS NULL AND blob_purged_at IS NULL
		  AND purpose = ANY($1) AND id > $2
		ORDER BY id LIMIT $3`, purposes, after, limit)
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

func (s *PostgresStore) SetImageSizes(ctx context.Context, id uuid.UUID, size ImageSize, objects map[string]SizeObject) error {
	column, err := sizeObjectsColumn(objects)
	if err != nil {
		return err
	}
	tag, err := s.pool.Exec(ctx, `UPDATE media SET width = $2, height = $3, size_objects = $4::jsonb
		WHERE id = $1 AND blob_purge_started_at IS NULL AND blob_purged_at IS NULL`,
		id, positiveOrNil(size.Width), positiveOrNil(size.Height), column)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		m, err := s.GetIncludingDeleted(ctx, id)
		if err != nil {
			return err
		}
		if m.BlobPurgedAt != nil {
			return ErrPurged
		}
		return ErrPurgeInProgress
	}
	return nil
}
