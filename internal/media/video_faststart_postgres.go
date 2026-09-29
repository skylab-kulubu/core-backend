package media

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// FaststartClaim is a faststart worker's claim on one video: the Media as
// it was claimed, where its rewrite was (none, or FaststartMoved), the
// claim's id and lease, and the steps of it that failed in a row before.
// Only the worker holding the claim moves the Media on; the archive and
// expiry purges wait for the lease.
//
// Leases are compared with each replica's own clock, as the scan's are.
// Where replicas' clocks disagree beyond the lease margin (two minutes),
// two workers may rewrite the same video at once; each writes and deletes
// only its own claim's copy key, so neither can delete the copy the Media
// points at, and no original goes before that copy is checked there.
type FaststartClaim struct {
	Media    Media
	State    FaststartState
	ID       uuid.UUID
	Until    time.Time
	Attempts int
}

// withExtra scans a row of mediaCols and then the extra columns.
type withExtra struct {
	row   rowScanner
	extra []any
}

func (w withExtra) Scan(dest ...any) error { return w.row.Scan(append(dest, w.extra...)...) }

// ClaimNextFaststart claims, in one short transaction that commits before
// any storage work, the first video after the given id the faststart
// worker has work for at now: a current public video Media waiting for its
// rewrite, or moved to its copy and waiting for its original to go, whose
// retry time has come and that no live claim holds. A row another
// transaction has locked is skipped (FOR UPDATE SKIP LOCKED). The lease is
// now plus lease(Media). found is false when there is none.
func (s *PostgresStore) ClaimNextFaststart(ctx context.Context, now time.Time, after uuid.UUID, lease func(Media) time.Duration) (claim FaststartClaim, found bool, err error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return FaststartClaim{}, false, err
	}
	defer tx.Rollback(ctx)
	// The conditions start with media_video_faststart_due_idx's predicate,
	// so the planner can use the partial index.
	var state string
	m, err := scanMedia(withExtra{row: tx.QueryRow(ctx, `SELECT `+mediaCols+`, video_faststart_attempts, COALESCE(video_faststart, '') FROM media
		WHERE purpose = '`+PurposeVideo+`' AND (video_faststart IS NULL OR video_faststart = '`+string(FaststartMoved)+`') AND blob_purged_at IS NULL AND id > $2
		  AND deleted_at IS NULL AND blob_purge_started_at IS NULL
		  AND visibility = 'public' AND status NOT IN ('scanning', 'rejected')
		  AND (video_faststart_retry_at IS NULL OR video_faststart_retry_at <= $1)
		  AND (video_faststart_claimed_until IS NULL OR video_faststart_claimed_until <= $1)
		ORDER BY id LIMIT 1
		FOR UPDATE SKIP LOCKED`, now, after), extra: []any{&claim.Attempts, &state}})
	if errors.Is(err, pgx.ErrNoRows) {
		return FaststartClaim{}, false, nil
	}
	if err != nil {
		return FaststartClaim{}, false, err
	}
	claim.Media, claim.State, claim.ID, claim.Until = m, FaststartState(state), uuid.New(), now.Add(lease(m))
	if _, err := tx.Exec(ctx, `UPDATE media SET video_faststart_claim_id = $2, video_faststart_claimed_until = $3 WHERE id = $1`,
		m.ID, claim.ID, claim.Until); err != nil {
		return FaststartClaim{}, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return FaststartClaim{}, false, err
	}
	return claim, true, nil
}

// MoveToFaststart points the Media at its checked faststart copy (key, of
// size bytes) under its claim, and lets go of the claim: the Media is
// moved, due again FaststartOriginalGrace after now, when its original
// goes. moved is false, and nothing changes, when the claim is no longer
// this one or the Media is no longer current at the key it was claimed at
// (archived, being purged, purged).
func (s *PostgresStore) MoveToFaststart(ctx context.Context, claim FaststartClaim, key string, size int64, now time.Time) (moved bool, err error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE media SET
			file_url = $4, file_size = $5, updated_at = $6, video_faststart = '`+string(FaststartMoved)+`',
			video_faststart_attempts = 0, video_faststart_retry_at = $7,
			video_faststart_claim_id = NULL, video_faststart_claimed_until = NULL
		WHERE id = $1 AND video_faststart_claim_id = $2 AND file_url = $3 AND video_faststart IS NULL AND `+currentSQL,
		claim.Media.ID, claim.ID, claim.Media.Key, key, size, now, now.Add(FaststartOriginalGrace))
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// MoveBackFromFaststart points a moved Media back at its original (key, of
// size bytes) under its claim, when its faststart copy is not there whole:
// it waits for its rewrite again, due at once. back is false, and nothing
// changes, when the claim is no longer this one, or the Media is no longer
// moved at the key it was claimed at, or its purge has begun.
func (s *PostgresStore) MoveBackFromFaststart(ctx context.Context, claim FaststartClaim, key string, size int64, now time.Time) (back bool, err error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE media SET
			file_url = $4, file_size = $5, updated_at = $6, video_faststart = NULL,
			video_faststart_attempts = 0, video_faststart_retry_at = NULL,
			video_faststart_claim_id = NULL, video_faststart_claimed_until = NULL
		WHERE id = $1 AND video_faststart_claim_id = $2 AND file_url = $3 AND video_faststart = '`+string(FaststartMoved)+`'
		  AND blob_purge_started_at IS NULL AND blob_purged_at IS NULL`,
		claim.Media.ID, claim.ID, claim.Media.Key, key, size, now)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// FaststartCopyDisposable reports whether the job holding claim may
// delete the copy it wrote at key, its claim's own: unless the Media points
// at it. Only that job moves the Media there, and no other writes the key.
// A Media that is gone points at nothing.
func (s *PostgresStore) FaststartCopyDisposable(ctx context.Context, claim FaststartClaim, key string) (bool, error) {
	var points bool
	err := s.pool.QueryRow(ctx, `SELECT file_url = $2 FROM media WHERE id = $1`, claim.Media.ID, key).Scan(&points)
	if errors.Is(err, pgx.ErrNoRows) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return !points, nil
}

// FaststartClaimHolds reports whether claim is still the video's, and the
// key the Media points at: a sweep of its copies checks it after listing
// them.
func (s *PostgresStore) FaststartClaimHolds(ctx context.Context, claim FaststartClaim) (pointed string, holds bool, err error) {
	err = s.pool.QueryRow(ctx, `SELECT file_url FROM media WHERE id = $1 AND video_faststart_claim_id = $2`,
		claim.Media.ID, claim.ID).Scan(&pointed)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	return pointed, err == nil, err
}

// FinishFaststart ends a video's rewrite as state under its claim, and lets
// go of the claim. done is false, and the state does not change, when the
// claim is no longer this one or the Media is purged (its claim is still
// let go).
func (s *PostgresStore) FinishFaststart(ctx context.Context, claim FaststartClaim, state FaststartState) (done bool, err error) {
	err = s.pool.QueryRow(ctx, `
		UPDATE media SET
			video_faststart = CASE WHEN blob_purged_at IS NULL THEN $3 ELSE video_faststart END,
			video_faststart_attempts = CASE WHEN blob_purged_at IS NULL THEN 0 ELSE video_faststart_attempts END,
			video_faststart_retry_at = NULL,
			video_faststart_claim_id = NULL, video_faststart_claimed_until = NULL
		WHERE id = $1 AND video_faststart_claim_id = $2
		RETURNING blob_purged_at IS NULL`, claim.Media.ID, claim.ID, string(state)).Scan(&done)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return done, err
}

// DeferFaststart puts off a video whose step failed, and lets go of its
// claim: each failure in a row doubles the wait, from faststartRetryFirst up
// to faststartRetryMax. giveUp fails it instead: it is served as it is. A
// claim no longer this one changes nothing.
func (s *PostgresStore) DeferFaststart(ctx context.Context, claim FaststartClaim, now time.Time, giveUp bool) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE media SET
			video_faststart = CASE WHEN $6 THEN '`+string(FaststartFailed)+`' ELSE video_faststart END,
			video_faststart_retry_at = CASE WHEN $6 THEN NULL
				ELSE $3::TIMESTAMPTZ + LEAST($4::INTERVAL * power(2, LEAST(video_faststart_attempts, 20)), $5::INTERVAL) END,
			video_faststart_attempts = video_faststart_attempts + 1,
			video_faststart_claim_id = NULL, video_faststart_claimed_until = NULL
		WHERE id = $1 AND video_faststart_claim_id = $2`,
		claim.Media.ID, claim.ID, now, faststartRetryFirst, faststartRetryMax, giveUp)
	return err
}

// FailFaststart fails a video whose step panicked, counting the attempt,
// and lets go of its claim: it is served as it is, and never tried again.
// A claim no longer this one changes nothing.
func (s *PostgresStore) FailFaststart(ctx context.Context, claim FaststartClaim) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE media SET
			video_faststart = '`+string(FaststartFailed)+`', video_faststart_attempts = video_faststart_attempts + 1,
			video_faststart_retry_at = NULL, video_faststart_claim_id = NULL, video_faststart_claimed_until = NULL
		WHERE id = $1 AND video_faststart_claim_id = $2`, claim.Media.ID, claim.ID)
	return err
}

// ReleaseFaststart lets go of a claim that moved nothing, counting nothing
// against the video.
func (s *PostgresStore) ReleaseFaststart(ctx context.Context, claim FaststartClaim) error {
	_, err := s.pool.Exec(ctx, `UPDATE media SET video_faststart_claim_id = NULL, video_faststart_claimed_until = NULL
		WHERE id = $1 AND video_faststart_claim_id = $2`, claim.Media.ID, claim.ID)
	return err
}
