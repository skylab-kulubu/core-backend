package media

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// FaststartClaim is a faststart worker's claim on one video: the Media as
// it was claimed, the claim's id and lease, and the rewrites of it that
// failed in a row before. Only the worker holding the claim moves the Media
// on; the archive and expiry purges wait for the lease.
type FaststartClaim struct {
	Media    Media
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
// worker has work for at now: a current public video Media whose rewrite
// has not ended, whose retry time has come and that no live claim holds. A
// row another transaction has locked is skipped (FOR UPDATE SKIP LOCKED).
// The lease is now plus lease(Media). found is false when there is none.
func (s *PostgresStore) ClaimNextFaststart(ctx context.Context, now time.Time, after uuid.UUID, lease func(Media) time.Duration) (claim FaststartClaim, found bool, err error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return FaststartClaim{}, false, err
	}
	defer tx.Rollback(ctx)
	// The conditions start with media_video_faststart_due_idx's predicate,
	// so the planner can use the partial index.
	m, err := scanMedia(withExtra{row: tx.QueryRow(ctx, `SELECT `+mediaCols+`, video_faststart_attempts FROM media
		WHERE purpose = '`+PurposeVideo+`' AND video_faststart IS NULL AND blob_purged_at IS NULL AND id > $2
		  AND deleted_at IS NULL AND blob_purge_started_at IS NULL
		  AND visibility = 'public' AND status NOT IN ('scanning', 'rejected')
		  AND (video_faststart_retry_at IS NULL OR video_faststart_retry_at <= $1)
		  AND (video_faststart_claimed_until IS NULL OR video_faststart_claimed_until <= $1)
		ORDER BY id LIMIT 1
		FOR UPDATE SKIP LOCKED`, now, after), extra: []any{&claim.Attempts}})
	if errors.Is(err, pgx.ErrNoRows) {
		return FaststartClaim{}, false, nil
	}
	if err != nil {
		return FaststartClaim{}, false, err
	}
	claim.Media, claim.ID, claim.Until = m, uuid.New(), now.Add(lease(m))
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
// size bytes) under its claim, and lets go of the claim: the Media is due
// again FaststartOriginalGrace after now, when its original goes. moved is
// false, and nothing changes, when the claim is no longer this one or the
// Media is no longer current at the key it was claimed at (archived, being
// purged, purged).
func (s *PostgresStore) MoveToFaststart(ctx context.Context, claim FaststartClaim, key string, size int64, now time.Time) (moved bool, err error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE media SET
			file_url = $4, file_size = $5, updated_at = $6,
			video_faststart_attempts = 0, video_faststart_retry_at = $7,
			video_faststart_claim_id = NULL, video_faststart_claimed_until = NULL
		WHERE id = $1 AND video_faststart_claim_id = $2 AND file_url = $3 AND video_faststart IS NULL AND `+currentSQL,
		claim.Media.ID, claim.ID, claim.Media.Key, key, size, now, now.Add(FaststartOriginalGrace))
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// FinishFaststart ends a video's rewrite as state under its claim, and lets
// go of the claim. done is false, and nothing changes, when the claim is no
// longer this one.
func (s *PostgresStore) FinishFaststart(ctx context.Context, claim FaststartClaim, state FaststartState) (done bool, err error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE media SET
			video_faststart = $3, video_faststart_attempts = 0, video_faststart_retry_at = NULL,
			video_faststart_claim_id = NULL, video_faststart_claimed_until = NULL
		WHERE id = $1 AND video_faststart_claim_id = $2`, claim.Media.ID, claim.ID, string(state))
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// DeferFaststart puts off a video whose step failed, and lets go of its
// claim: each failure in a row doubles the wait, from faststartRetryFirst up
// to faststartRetryMax. giveUp fails it instead: it is served as it is. A
// claim no longer this one changes nothing.
func (s *PostgresStore) DeferFaststart(ctx context.Context, claim FaststartClaim, now time.Time, giveUp bool) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE media SET
			video_faststart = CASE WHEN $6 THEN '`+string(FaststartFailed)+`' END,
			video_faststart_retry_at = CASE WHEN $6 THEN NULL
				ELSE $3::TIMESTAMPTZ + LEAST($4::INTERVAL * power(2, LEAST(video_faststart_attempts, 20)), $5::INTERVAL) END,
			video_faststart_attempts = video_faststart_attempts + 1,
			video_faststart_claim_id = NULL, video_faststart_claimed_until = NULL
		WHERE id = $1 AND video_faststart_claim_id = $2`,
		claim.Media.ID, claim.ID, now, faststartRetryFirst, faststartRetryMax, giveUp)
	return err
}

// ReleaseFaststart lets go of a claim that moved nothing, counting nothing
// against the video.
func (s *PostgresStore) ReleaseFaststart(ctx context.Context, claim FaststartClaim) error {
	_, err := s.pool.Exec(ctx, `UPDATE media SET video_faststart_claim_id = NULL, video_faststart_claimed_until = NULL
		WHERE id = $1 AND video_faststart_claim_id = $2`, claim.Media.ID, claim.ID)
	return err
}
