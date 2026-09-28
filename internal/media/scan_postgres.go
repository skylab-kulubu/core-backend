package media

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ListScanDue returns, in id order and after the given id, the Media the
// scan worker has work for at now, once their retry time has come: current
// Media waiting for their scan, and rejected Media whose objects are not
// deleted yet. An archived Media waits for its restore (or its purge); a
// Media whose purge started is the purge's.
func (s *PostgresStore) ListScanDue(ctx context.Context, now time.Time, after uuid.UUID, limit int) ([]Media, error) {
	if limit <= 0 {
		limit = 25
	}
	// The conditions repeat media_scan_due_idx's predicate, so the planner
	// can use the partial index.
	rows, err := s.pool.Query(ctx, `SELECT `+mediaCols+` FROM media
		WHERE status IN ('scanning', 'rejected') AND blob_purged_at IS NULL AND id > $2
		  AND (status = 'rejected' OR (deleted_at IS NULL AND blob_purge_started_at IS NULL))
		  AND (scan_retry_at IS NULL OR scan_retry_at <= $1)
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

// scanningCurrentSQL holds for a current Media waiting for its scan, at the
// object key $2.
const scanningCurrentSQL = `status = 'scanning' AND file_url = $2 AND ` + currentSQL

// MarkScanClean ends the scan of a Media found clean, stored at heldKey,
// and records the object at servedKey (heldKey itself, unless the file was
// held apart until clean). The Media takes the status its Media
// attachments give it: attached when one links it, with no expiry;
// pending otherwise, keeping its expiry. done is false, and nothing
// changes, when the Media is no longer waiting for its scan there (purged,
// archived, or moved on).
func (s *PostgresStore) MarkScanClean(ctx context.Context, id uuid.UUID, heldKey, servedKey string, now time.Time) (done bool, err error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	// The Media's row lock first, as the status trigger takes it: the
	// statement after it then sees every Media attachment committed before
	// it, and one written after it finds the Media clean.
	if _, err := tx.Exec(ctx, `SELECT 1 FROM media WHERE id = $1 FOR NO KEY UPDATE`, id); err != nil {
		return false, err
	}
	tag, err := tx.Exec(ctx, `
		UPDATE media SET
			status = CASE WHEN EXISTS (SELECT 1 FROM media_attachments a WHERE a.media_id = media.id) THEN 'attached' ELSE 'pending' END,
			expires_at = CASE WHEN EXISTS (SELECT 1 FROM media_attachments a WHERE a.media_id = media.id) THEN NULL ELSE expires_at END,
			scan_result = 'clean', scan_attempts = 0, scan_retry_at = NULL,
			file_url = $3, updated_at = $4
		WHERE id = $1 AND `+scanningCurrentSQL, id, heldKey, servedKey, now)
	if err != nil {
		return false, err
	}
	if tag.RowsAffected() == 0 {
		return false, nil
	}
	return true, tx.Commit(ctx)
}

// RejectScanned rejects a Media waiting for its scan at key: its status
// and why (result), no expiry, and the claim that its object is being
// deleted, which refuses new Media attachments and restores as a purge's
// claim does; and the rejection's record, with the signature clamd named.
// done is false, and nothing changes, when the Media is no longer waiting
// for its scan there. FinishScanRejection deletes the object.
func (s *PostgresStore) RejectScanned(ctx context.Context, id uuid.UUID, key string, result ScanResult, signature string, now time.Time) (done bool, err error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	tag, err := tx.Exec(ctx, `
		UPDATE media SET
			status = 'rejected', scan_result = $3, expires_at = NULL, scan_retry_at = NULL,
			blob_purge_started_at = $4, blob_purge_checked_at = $4, updated_at = $4
		WHERE id = $1 AND `+scanningCurrentSQL, id, key, string(result), now)
	if err != nil {
		return false, err
	}
	if tag.RowsAffected() == 0 {
		return false, nil
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO media_scan_rejections (media_id, result, signature, rejected_at) VALUES ($1, $2, $3, $4)
		ON CONFLICT (media_id) DO NOTHING`, id, string(result), signature, now); err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}

// FinishScanRejection deletes the objects of a rejected Media and records
// them purged. Deleting is idempotent, so a rejection cut short (a crash,
// a storage failure) is finished by the next pass. The claim is taken again
// should another purge have let it go meanwhile. done is false when there
// is nothing left to delete.
func (s *PostgresStore) FinishScanRejection(ctx context.Context, id uuid.UUID, now time.Time, purge func(key string) error) (done bool, err error) {
	var key string
	err = s.pool.QueryRow(ctx, `
		UPDATE media SET blob_purge_started_at = COALESCE(blob_purge_started_at, $2), blob_purge_checked_at = $2
		WHERE id = $1 AND status = 'rejected' AND blob_purged_at IS NULL
		RETURNING file_url`, id, now).Scan(&key)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := purgeObjects(key, purge); err != nil {
		return false, err
	}
	_, err = s.pool.Exec(ctx, `
		UPDATE media SET blob_purged_at = $2, blob_purge_checked_at = $2, updated_at = $2
		WHERE id = $1 AND status = 'rejected' AND blob_purged_at IS NULL`, id, now)
	return err == nil, err
}

// DeferScan puts off a Media whose scan step failed (clamd answered an
// error, storage could not read the file or delete a rejected one's): each
// failure in a row doubles the wait, from scanRetryFirst up to
// scanRetryMax.
func (s *PostgresStore) DeferScan(ctx context.Context, id uuid.UUID, now time.Time) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE media SET
			scan_retry_at = $2::TIMESTAMPTZ + LEAST($3::INTERVAL * power(2, LEAST(scan_attempts, 20)), $4::INTERVAL),
			scan_attempts = scan_attempts + 1
		WHERE id = $1 AND status IN ('scanning', 'rejected')`, id, now, scanRetryFirst, scanRetryMax)
	return err
}

// ScanRejection is the record of one rejection: which Media, why, the name
// clamd gave what it found, and when. It names no file and no person.
type ScanRejection struct {
	MediaID    uuid.UUID
	Result     ScanResult
	Signature  string
	RejectedAt time.Time
}

// GetScanRejection returns the Media's rejection record; ErrNotFound when
// the scan did not reject it.
func (s *PostgresStore) GetScanRejection(ctx context.Context, id uuid.UUID) (ScanRejection, error) {
	var r ScanRejection
	var result string
	err := s.pool.QueryRow(ctx, `SELECT media_id, result, signature, rejected_at FROM media_scan_rejections WHERE media_id = $1`, id).
		Scan(&r.MediaID, &result, &r.Signature, &r.RejectedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return ScanRejection{}, ErrNotFound
	}
	r.Result = ScanResult(result)
	return r, err
}
