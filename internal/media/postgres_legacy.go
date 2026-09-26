package media

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// legacyAttachedByCoreSQL holds for a legacy Media that at least one of
// core's own Media attachments uses.
const legacyAttachedByCoreSQL = `media.purpose = 'legacy'
	AND EXISTS (SELECT 1 FROM media_attachments a WHERE a.media_id = media.id AND a.owner_service = 'core')`

// listLegacyAttachedByCore returns, in id order and after the given id, the
// legacy Media core attaches.
func (s *PostgresStore) listLegacyAttachedByCore(ctx context.Context, after uuid.UUID, limit int) ([]uuid.UUID, error) {
	rows, err := s.pool.Query(ctx, `SELECT id FROM media
		WHERE `+legacyAttachedByCoreSQL+` AND id > $1
		ORDER BY id LIMIT $2`, after, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
}

func (s *PostgresStore) countLegacyAttachedByCore(ctx context.Context) (int, error) {
	var count int
	err := s.pool.QueryRow(ctx, `SELECT count(*) FROM media WHERE `+legacyAttachedByCoreSQL).Scan(&count)
	return count, err
}

// assignLegacyPurpose gives a legacy Media the purpose choose picks from its
// Media attachments, and returns it; "" when choose keeps it legacy or it is
// no longer legacy. The Media row is locked first: a new Media attachment
// checks its Media and waits, so choose sees every one.
func (s *PostgresStore) assignLegacyPurpose(ctx context.Context, id uuid.UUID, choose func([]attachmentUse) string) (string, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx)
	var locked uuid.UUID
	err = tx.QueryRow(ctx, `SELECT id FROM media WHERE id = $1 AND purpose = 'legacy' FOR UPDATE`, id).Scan(&locked)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	rows, err := tx.Query(ctx, `SELECT owner_service, role FROM media_attachments WHERE media_id = $1 ORDER BY id`, id)
	if err != nil {
		return "", err
	}
	uses, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (attachmentUse, error) {
		var use attachmentUse
		err := row.Scan(&use.Product, &use.Role)
		return use, err
	})
	if err != nil {
		return "", err
	}
	purpose := choose(uses)
	if purpose == "" {
		return "", nil
	}
	if _, err := tx.Exec(ctx, `UPDATE media SET purpose = $2 WHERE id = $1`, id, purpose); err != nil {
		return "", err
	}
	return purpose, tx.Commit(ctx)
}

// legacyOrphanSQL holds for a current legacy Media that nothing in core uses:
// no Media attachment, and no core link either (coreLinksSQL, the safety
// net until every link is proven to have its Media attachment).
const legacyOrphanSQL = `media.purpose = 'legacy' AND media.status <> 'attached'
	AND media.deleted_at IS NULL AND media.blob_purge_started_at IS NULL AND media.blob_purged_at IS NULL
	AND NOT EXISTS (SELECT 1 FROM media_attachments a WHERE a.media_id = media.id)`

// listLegacyOrphans returns every legacy orphan, oldest upload first.
func (s *PostgresStore) listLegacyOrphans(ctx context.Context) ([]Media, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+mediaCols+` FROM media
		WHERE `+legacyOrphanSQL+` AND NOT EXISTS (`+coreLinksSQL("media.id")+`)
		ORDER BY created_at, id`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Media, error) { return scanMedia(row) })
}

// countCoreLinksWithoutAttachment counts core's own links, read from the
// linking records as the Media attachment migration (20260926120000) read
// them, that have no Media attachment. Zero proves the purge's hard-coded list
// of core links (coreLinksSQL) adds nothing to the Media attachments.
func (s *PostgresStore) countCoreLinksWithoutAttachment(ctx context.Context) (int, error) {
	var count int
	err := s.pool.QueryRow(ctx, `SELECT count(*)
		FROM (
			SELECT 'event', 'event_cover', to_jsonb(owner), 'id', 'cover_image_id', NULL::TEXT FROM events owner
			UNION ALL
			SELECT 'event', 'event_gallery', to_jsonb(owner), 'event_id', 'media_id', NULL FROM event_images owner
			UNION ALL
			SELECT 'user', 'profile_picture', to_jsonb(owner), 'id', 'profile_picture_id', NULL FROM users owner
			UNION ALL
			SELECT 'certificate_template', 'certificate_asset', to_jsonb(owner), 'id', 'draft_layout', NULL FROM certificate_templates owner
			UNION ALL
			SELECT 'certificate_template_version', 'certificate_asset', to_jsonb(owner), 'id', 'layout', 'asset_manifest'
			FROM certificate_template_versions owner
		) source (owner_type, role, record, owner_column, media_column, manifest_column)
		CROSS JOIN LATERAL core_media_links(source.record, source.owner_column, source.media_column, source.manifest_column) link
		JOIN media ON media.id = link.media_id
		WHERE NOT EXISTS (
			SELECT 1 FROM media_attachments a
			WHERE a.media_id = link.media_id AND a.owner_service = 'core' AND a.owner_type = source.owner_type
			  AND a.owner_id = link.owner_id::TEXT AND a.role = source.role
		)`).Scan(&count)
	return count, err
}

// orphanState is what expireLegacyOrphan found.
type orphanState int

const (
	notAnOrphan orphanState = iota
	orphanExpiring
	orphanAlreadyExpiring
)

// expireLegacyOrphan sets the expiry of a legacy orphan that has none to at;
// without apply it only looks. The Media row is locked as it is checked: a
// Media attached meanwhile is attached by then, and its new row fails the
// check.
func (s *PostgresStore) expireLegacyOrphan(ctx context.Context, id uuid.UUID, at time.Time, apply bool) (orphanState, error) {
	orphan := legacyOrphanSQL + ` AND NOT EXISTS (` + coreLinksSQL("media.id") + `)`
	var expiring bool
	var err error
	if apply {
		err = s.pool.QueryRow(ctx, `WITH target AS (
				SELECT id, expires_at FROM media WHERE id = $1 AND `+orphan+` FOR UPDATE
			)
			UPDATE media SET expires_at = COALESCE(target.expires_at, $2)
			FROM target WHERE media.id = target.id
			RETURNING target.expires_at IS NOT NULL`, id, at).Scan(&expiring)
	} else {
		err = s.pool.QueryRow(ctx, `SELECT expires_at IS NOT NULL FROM media WHERE id = $1 AND `+orphan, id).Scan(&expiring)
	}
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return notAnOrphan, nil
	case err != nil:
		return notAnOrphan, err
	case expiring:
		return orphanAlreadyExpiring, nil
	default:
		return orphanExpiring, nil
	}
}
