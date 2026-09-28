package media

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ScanClaim is a scan worker's claim on one Media: the Media as it was
// claimed, the claim's id, and its lease. Only the worker holding the claim
// moves the Media on; the archive and expiry purges wait for the lease.
type ScanClaim struct {
	Media Media
	ID    uuid.UUID
	Until time.Time
}

// ClaimNextScan claims, in one short transaction that commits before any
// clamd or storage work, the first Media after the given id the scan worker
// has work for at now: a current Media waiting for its scan, or a rejected
// one whose objects are not deleted yet, whose retry time has come and that
// no live claim holds. A row another transaction has locked is skipped
// (FOR UPDATE SKIP LOCKED). The lease is now plus lease(Media). found is
// false when there is none.
func (s *PostgresStore) ClaimNextScan(ctx context.Context, now time.Time, after uuid.UUID, lease func(Media) time.Duration) (claim ScanClaim, found bool, err error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return ScanClaim{}, false, err
	}
	defer tx.Rollback(ctx)
	// The conditions start with media_scan_due_idx's predicate, so the
	// planner can use the partial index.
	m, err := scanMedia(tx.QueryRow(ctx, `SELECT `+mediaCols+` FROM media
		WHERE status IN ('scanning', 'rejected') AND blob_purged_at IS NULL AND id > $2
		  AND (status = 'rejected' OR (deleted_at IS NULL AND blob_purge_started_at IS NULL))
		  AND (scan_retry_at IS NULL OR scan_retry_at <= $1)
		  AND (scan_claimed_until IS NULL OR scan_claimed_until <= $1)
		ORDER BY id LIMIT 1
		FOR UPDATE SKIP LOCKED`, now, after))
	if errors.Is(err, pgx.ErrNoRows) {
		return ScanClaim{}, false, nil
	}
	if err != nil {
		return ScanClaim{}, false, err
	}
	claim = ScanClaim{Media: m, ID: uuid.New(), Until: now.Add(lease(m))}
	if _, err := tx.Exec(ctx, `UPDATE media SET scan_claim_id = $2, scan_claimed_until = $3 WHERE id = $1`,
		m.ID, claim.ID, claim.Until); err != nil {
		return ScanClaim{}, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return ScanClaim{}, false, err
	}
	return claim, true, nil
}

// ReleaseScan lets go of a claim whose scan did not happen (clamd could not
// be reached): the Media is due again at once, and nothing counts against
// it.
func (s *PostgresStore) ReleaseScan(ctx context.Context, claim ScanClaim) error {
	_, err := s.pool.Exec(ctx, `UPDATE media SET scan_claim_id = NULL, scan_claimed_until = NULL
		WHERE id = $1 AND scan_claim_id = $2`, claim.Media.ID, claim.ID)
	return err
}

// MarkScanClean ends the scan of a Media found clean, under its claim. The
// Media records the object at served (the key it was claimed at, unless the
// file was held apart until clean) and takes the status its Media
// attachments give it: attached when one links it, with no expiry; pending
// otherwise, keeping its expiry. done is false, and nothing changes, when
// the claim is no longer this one or the Media no longer waits for its scan
// at that key (purged, archived, moved on by another worker).
func (s *PostgresStore) MarkScanClean(ctx context.Context, claim ScanClaim, served string, now time.Time) (done bool, err error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	// The Media's row lock first, as the status trigger takes it: the
	// statement after it then sees every Media attachment committed before
	// it, and one written after it finds the Media clean.
	if _, err := tx.Exec(ctx, `SELECT 1 FROM media WHERE id = $1 FOR NO KEY UPDATE`, claim.Media.ID); err != nil {
		return false, err
	}
	tag, err := tx.Exec(ctx, `
		UPDATE media SET
			status = CASE WHEN EXISTS (SELECT 1 FROM media_attachments a WHERE a.media_id = media.id) THEN 'attached' ELSE 'pending' END,
			expires_at = CASE WHEN EXISTS (SELECT 1 FROM media_attachments a WHERE a.media_id = media.id) THEN NULL ELSE expires_at END,
			scan_result = 'clean', scan_attempts = 0, scan_retry_at = NULL,
			scan_claim_id = NULL, scan_claimed_until = NULL,
			file_url = $4, updated_at = $5
		WHERE id = $1 AND scan_claim_id = $2 AND status = 'scanning' AND file_url = $3 AND `+currentSQL,
		claim.Media.ID, claim.ID, claim.Media.Key, served, now)
	if err != nil {
		return false, err
	}
	if tag.RowsAffected() == 0 {
		return false, nil
	}
	return true, tx.Commit(ctx)
}

// RejectScanned rejects a Media under its claim: its status and why
// (result), no expiry, the claim that its objects are being deleted (which
// refuses new Media attachments and restores as a purge's claim does), and
// the rejection's record, with the signature clamd named. The scan claim
// stays until FinishScanRejection has deleted the objects. done is false,
// and nothing changes, when the claim is no longer this one or the Media no
// longer waits for its scan at the key it was claimed at.
func (s *PostgresStore) RejectScanned(ctx context.Context, claim ScanClaim, result ScanResult, signature string, now time.Time) (done bool, err error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	tag, err := tx.Exec(ctx, `
		UPDATE media SET
			status = 'rejected', scan_result = $4, expires_at = NULL, scan_retry_at = NULL,
			blob_purge_started_at = $5, blob_purge_checked_at = $5, updated_at = $5
		WHERE id = $1 AND scan_claim_id = $2 AND status = 'scanning' AND file_url = $3 AND `+currentSQL,
		claim.Media.ID, claim.ID, claim.Media.Key, string(result), now)
	if err != nil {
		return false, err
	}
	if tag.RowsAffected() == 0 {
		return false, nil
	}
	if err := recordRejection(ctx, tx, claim.Media.ID, result, signature, now); err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}

func recordRejection(ctx context.Context, tx pgx.Tx, id uuid.UUID, result ScanResult, signature string, now time.Time) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO media_scan_rejections (media_id, result, signature, rejected_at) VALUES ($1, $2, $3, $4)
		ON CONFLICT (media_id) DO NOTHING`, id, string(result), signature, now)
	return err
}

// RejectOverdueScans rejects, as scan_timeout, every current Media still
// waiting for its scan deadline after its upload, that no live claim holds,
// and records each rejection. It needs no scanner, so it runs while clamd
// is down too. The scan worker then deletes their objects as it does any
// rejected Media's.
func (s *PostgresStore) RejectOverdueScans(ctx context.Context, now time.Time, deadline time.Duration) (int, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	rows, err := tx.Query(ctx, `
		UPDATE media SET
			status = 'rejected', scan_result = 'scan_timeout', expires_at = NULL, scan_retry_at = NULL,
			blob_purge_started_at = $1, blob_purge_checked_at = $1, updated_at = $1
		WHERE status = 'scanning' AND `+currentSQL+`
		  AND created_at <= $2
		  AND (scan_claimed_until IS NULL OR scan_claimed_until <= $1)
		RETURNING id`, now, now.Add(-deadline))
	if err != nil {
		return 0, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return 0, err
	}
	for _, id := range ids {
		if err := recordRejection(ctx, tx, id, ScanTimeout, "", now); err != nil {
			return 0, err
		}
	}
	return len(ids), tx.Commit(ctx)
}

// FinishScanRejection deletes the objects of a rejected Media and records
// them purged, letting go of its scan claim. Deleting is idempotent, so a
// rejection cut short (a crash, a storage failure) is finished by a later
// pass. The purge claim is taken again should another purge have let it go
// meanwhile. done is false when there is nothing left to delete.
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
	if err := purgeMediaObjects(id, key, purge); err != nil {
		return false, err
	}
	_, err = s.pool.Exec(ctx, `
		UPDATE media SET blob_purged_at = $2, blob_purge_checked_at = $2, updated_at = $2,
			scan_claim_id = NULL, scan_claimed_until = NULL
		WHERE id = $1 AND status = 'rejected' AND blob_purged_at IS NULL`, id, now)
	return err == nil, err
}

// DeferScan puts off a Media whose scan step failed (clamd answered an
// error, storage could not read the file or delete a rejected one's), and
// lets go of its claim: each failure in a row doubles the wait, from
// scanRetryFirst up to scanRetryMax. A claim no longer this one changes
// nothing.
func (s *PostgresStore) DeferScan(ctx context.Context, claim ScanClaim, now time.Time) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE media SET
			scan_retry_at = $3::TIMESTAMPTZ + LEAST($4::INTERVAL * power(2, LEAST(scan_attempts, 20)), $5::INTERVAL),
			scan_attempts = scan_attempts + 1,
			scan_claim_id = NULL, scan_claimed_until = NULL
		WHERE id = $1 AND scan_claim_id = $2`, claim.Media.ID, claim.ID, now, scanRetryFirst, scanRetryMax)
	return err
}
