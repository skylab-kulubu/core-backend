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

// assignLegacyPurpose gives a legacy Media the purpose choose decides on
// from its Media attachments, with its detach expiry held, and returns the
// decision; legacySkipped when it is no longer legacy. The Media row is
// locked first: a new Media attachment checks its Media under a lock that
// waits for this one (require_current_attached_media), so choose sees every
// Media attachment, and one written after sees the new purpose.
func (s *PostgresStore) assignLegacyPurpose(ctx context.Context, id uuid.UUID, choose func([]attachmentUse) (string, legacyDecision, error)) (legacyDecision, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return legacySkipped, err
	}
	defer tx.Rollback(ctx)
	var locked uuid.UUID
	err = tx.QueryRow(ctx, `SELECT id FROM media WHERE id = $1 AND purpose = 'legacy' FOR UPDATE`, id).Scan(&locked)
	if errors.Is(err, pgx.ErrNoRows) {
		return legacySkipped, nil
	}
	if err != nil {
		return legacySkipped, err
	}
	rows, err := tx.Query(ctx, `SELECT owner_service, role FROM media_attachments WHERE media_id = $1 ORDER BY id`, id)
	if err != nil {
		return legacySkipped, err
	}
	uses, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (attachmentUse, error) {
		var use attachmentUse
		err := row.Scan(&use.Product, &use.Role)
		return use, err
	})
	if err != nil {
		return legacySkipped, err
	}
	purpose, decision, err := choose(uses)
	if err != nil || decision != legacyAssigned {
		return decision, err
	}
	hold, err := holdNotReleased(ctx, tx)
	if err != nil {
		return legacySkipped, err
	}
	if _, err := tx.Exec(ctx, `UPDATE media SET purpose = $2, detach_expiry_held = $3 WHERE id = $1`, id, purpose, hold); err != nil {
		return legacySkipped, err
	}
	return decision, tx.Commit(ctx)
}

// holdNotReleased reports whether the backfill still holds the detach expiry
// of the Media it gives a purpose, reading the release under a share lock
// that the release's update waits for.
func holdNotReleased(ctx context.Context, tx pgx.Tx) (bool, error) {
	var released *time.Time
	err := tx.QueryRow(ctx, `SELECT released_at FROM media_legacy_hold FOR SHARE`).Scan(&released)
	if errors.Is(err, pgx.ErrNoRows) {
		return true, nil
	}
	return released == nil, err
}

// recordHoldRelease records the release of the hold, keeping the time of
// the first one.
func (s *PostgresStore) recordHoldRelease(ctx context.Context, at time.Time) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO media_legacy_hold (released_at) VALUES ($1)
		ON CONFLICT (singleton) DO UPDATE SET released_at = COALESCE(media_legacy_hold.released_at, EXCLUDED.released_at)`, at)
	return err
}

// holdReleasedAt is when the hold was released; nil before.
func (s *PostgresStore) holdReleasedAt(ctx context.Context) (*time.Time, error) {
	var released *time.Time
	err := s.pool.QueryRow(ctx, `SELECT released_at FROM media_legacy_hold`).Scan(&released)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return released, err
}

// legacyOrphanSQL holds for a current legacy Media that nothing in core uses:
// no Media attachment, and no core link either (coreLinksSQL, the safety
// net until every link is proven to have its Media attachment).
const legacyOrphanSQL = `media.purpose = 'legacy' AND ` + unattachedCurrentSQL + `
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

// countCoreLinksWithoutAttachment counts core's own links (coreLinkSources)
// that have no Media attachment. Zero proves the purge's hard-coded list of
// core links adds nothing to the Media attachments.
func (s *PostgresStore) countCoreLinksWithoutAttachment(ctx context.Context) (int, error) {
	var count int
	err := s.pool.QueryRow(ctx, `SELECT count(*)
		FROM (`+coreLinkRowsSQL()+`) link
		JOIN media ON media.id = link.media_id
		WHERE NOT EXISTS (
			SELECT 1 FROM media_attachments a
			WHERE a.media_id = link.media_id AND a.owner_service = 'core' AND a.owner_type = link.owner_type
			  AND a.owner_id = link.owner_id::TEXT AND a.role = link.role
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

// windowAtReleaseSQL holds for a held Media whose 30 days the release
// starts: current, used by no record, with no expiry, and with a purpose. A
// held Media a product's attach gave back legacy keeps legacy's rule: no
// expiry but the orphan switch's.
const windowAtReleaseSQL = unattachedCurrentSQL + ` AND media.expires_at IS NULL AND media.purpose <> 'legacy'`

// countDetachExpiryHeld counts the held Media, and those of them whose 30
// days the release starts.
func (s *PostgresStore) countDetachExpiryHeld(ctx context.Context) (held, unattached int, err error) {
	err = s.pool.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE `+windowAtReleaseSQL+`)
		FROM media WHERE media.detach_expiry_held`).Scan(&held, &unattached)
	return held, unattached, err
}

// listDetachExpiryHeld returns, in id order and after the given id, the
// held Media.
func (s *PostgresStore) listDetachExpiryHeld(ctx context.Context, after uuid.UUID, limit int) ([]uuid.UUID, error) {
	rows, err := s.pool.Query(ctx, `SELECT id FROM media WHERE detach_expiry_held AND id > $1 ORDER BY id LIMIT $2`, after, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
}

// releaseDetachExpiryHold clears the Media's hold and, when no record uses
// it, it has no expiry and it has a purpose, sets its expiry to at. The Media row is locked as
// it is read, so a detach waits and then sees the hold gone (and sets the
// ordinary 30 days), or has already detached it for this to see.
func (s *PostgresStore) releaseDetachExpiryHold(ctx context.Context, id uuid.UUID, at time.Time) (released, windowStarted bool, err error) {
	err = s.pool.QueryRow(ctx, `WITH target AS (
			SELECT id, (`+windowAtReleaseSQL+`) AS starts
			FROM media WHERE id = $1 AND detach_expiry_held FOR UPDATE
		)
		UPDATE media SET detach_expiry_held = false,
			expires_at = CASE WHEN target.starts THEN $2 ELSE media.expires_at END
		FROM target WHERE media.id = target.id
		RETURNING target.starts`, id, at).Scan(&windowStarted)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, false, nil
	}
	return err == nil, windowStarted, err
}
