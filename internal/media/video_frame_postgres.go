package media

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// FrameClaim is a frame worker's claim on one Event video
// (event_videos, migration 20260929160000): the video's link, its Media's
// key as claimed, the claim's id and lease, and the tries that failed in a
// row before. Only the worker holding the claim writes the video's frame.
// Leases are compared with each replica's own clock, as the scan's and the
// faststart's are.
type FrameClaim struct {
	EventID  uuid.UUID
	VideoID  uuid.UUID
	VideoKey string
	ID       uuid.UUID
	Until    time.Time
	Attempts int
}

// frameAfter is where a pass is in its walk of the videos: by Event, then
// by video.
type frameAfter struct {
	eventID, videoID uuid.UUID
}

// frameSettledSQL are the faststart states a video's frame waits for: its
// key moves once, when faststart moves it to its copy (moved), and never
// again (done, not_needed); a failed rewrite is served as it is.
const frameSettledSQL = `('` + string(FaststartMoved) + `', '` + string(FaststartDone) + `', '` + string(FaststartNotNeeded) + `', '` + string(FaststartFailed) + `')`

// ClaimNextFrame claims, in one short transaction that commits before the
// frame is asked for, the first Event video after the given one that
// needs a frame at now: no poster uploaded, no frame taken, not given up
// on, its retry time come and no live claim on it; the Event current; the
// video's Media current, servable (ServableSQL) and settled by faststart. A
// row another transaction has locked is skipped (FOR UPDATE SKIP LOCKED).
// found is false when there is none.
func (s *PostgresStore) ClaimNextFrame(ctx context.Context, now time.Time, after frameAfter, lease time.Duration) (claim FrameClaim, found bool, err error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return FrameClaim{}, false, err
	}
	defer tx.Rollback(ctx)
	// The conditions start with event_videos_frame_due_idx's predicate.
	err = tx.QueryRow(ctx, `SELECT ev.event_id, ev.media_id, ev.frame_attempts, m.file_url
		FROM event_videos ev
		JOIN events e ON e.id = ev.event_id AND e.archived_at IS NULL
		JOIN media m ON m.id = ev.media_id
		WHERE ev.frame_media_id IS NULL AND ev.poster_media_id IS NULL AND ev.frame_state IS NULL
		  AND (ev.event_id, ev.media_id) > ($2, $3)
		  AND (ev.frame_retry_at IS NULL OR ev.frame_retry_at <= $1)
		  AND (ev.frame_claimed_until IS NULL OR ev.frame_claimed_until <= $1)
		  AND m.purpose = '`+PurposeVideo+`' AND m.deleted_at IS NULL AND `+ServableSQL("m")+`
		  AND m.video_faststart IN `+frameSettledSQL+`
		ORDER BY ev.event_id, ev.media_id LIMIT 1
		FOR UPDATE OF ev SKIP LOCKED`, now, after.eventID, after.videoID).
		Scan(&claim.EventID, &claim.VideoID, &claim.Attempts, &claim.VideoKey)
	if errors.Is(err, pgx.ErrNoRows) {
		return FrameClaim{}, false, nil
	}
	if err != nil {
		return FrameClaim{}, false, err
	}
	claim.ID, claim.Until = uuid.New(), now.Add(lease)
	if _, err := tx.Exec(ctx, `UPDATE event_videos SET frame_claim_id = $3, frame_claimed_until = $4
		WHERE event_id = $1 AND media_id = $2`, claim.EventID, claim.VideoID, claim.ID, claim.Until); err != nil {
		return FrameClaim{}, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return FrameClaim{}, false, err
	}
	return claim, true, nil
}

// CreateFrame stores the record of a frame whose objects are about to be
// written: pending, with its purpose's expiry, and no uploader. Written
// first, it finds the objects should the worker stop before it links the
// frame: the expiry cleanup purges them with it.
func (s *PostgresStore) CreateFrame(ctx context.Context, m Media) (Media, error) {
	m.UploadedBy = uuid.Nil
	return insertMedia(ctx, s.pool, newRecord(m))
}

// AttachFrame links the frame to the claimed video, under the Event's lock
// (taken first, as core's own writers do), and lets go of the claim: the
// statement's trigger writes the frame's Media attachment, checked as every
// new link is. attached is false, and nothing changes, when the claim is no
// longer this one, the video left the Event, or it has a frame already.
func (s *PostgresStore) AttachFrame(ctx context.Context, claim FrameClaim, frameID uuid.UUID) (attached bool, err error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT 1 FROM events WHERE id = $1 FOR NO KEY UPDATE`, claim.EventID); err != nil {
		return false, err
	}
	tag, err := tx.Exec(ctx, `UPDATE event_videos SET
			frame_media_id = $4, frame_attempts = 0, frame_retry_at = NULL,
			frame_claim_id = NULL, frame_claimed_until = NULL
		WHERE event_id = $1 AND media_id = $2 AND frame_claim_id = $3 AND frame_media_id IS NULL`,
		claim.EventID, claim.VideoID, claim.ID, frameID)
	if refusal, ok := DatabaseLinkRefusal(err); ok {
		return false, refusal
	}
	if err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// DeferFrame puts off a video whose frame failed, and lets go of its
// claim: each failure in a row doubles the wait, from frameRetryFirst up to
// frameRetryMax. giveUp fails it instead: it gets no frame. A claim no
// longer this one changes nothing.
func (s *PostgresStore) DeferFrame(ctx context.Context, claim FrameClaim, now time.Time, giveUp bool) error {
	_, err := s.pool.Exec(ctx, `UPDATE event_videos SET
			frame_state = CASE WHEN $6 THEN 'failed' ELSE frame_state END,
			frame_retry_at = CASE WHEN $6 THEN NULL
				ELSE $4::TIMESTAMPTZ + LEAST($5::INTERVAL * power(2, LEAST(frame_attempts, 20)), $7::INTERVAL) END,
			frame_attempts = frame_attempts + 1,
			frame_claim_id = NULL, frame_claimed_until = NULL
		WHERE event_id = $1 AND media_id = $2 AND frame_claim_id = $3`,
		claim.EventID, claim.VideoID, claim.ID, now, frameRetryFirst, giveUp, frameRetryMax)
	return err
}

// FailFrame gives up on a video at once, counting the try: the frame
// service takes no frame from it, or its step panicked. A claim no longer
// this one changes nothing.
func (s *PostgresStore) FailFrame(ctx context.Context, claim FrameClaim) error {
	_, err := s.pool.Exec(ctx, `UPDATE event_videos SET
			frame_state = 'failed', frame_attempts = frame_attempts + 1, frame_retry_at = NULL,
			frame_claim_id = NULL, frame_claimed_until = NULL
		WHERE event_id = $1 AND media_id = $2 AND frame_claim_id = $3`, claim.EventID, claim.VideoID, claim.ID)
	return err
}

// ReleaseFrame lets go of a claim, counting nothing against the video: the
// frame service could not be asked.
func (s *PostgresStore) ReleaseFrame(ctx context.Context, claim FrameClaim) error {
	_, err := s.pool.Exec(ctx, `UPDATE event_videos SET frame_claim_id = NULL, frame_claimed_until = NULL
		WHERE event_id = $1 AND media_id = $2 AND frame_claim_id = $3`, claim.EventID, claim.VideoID, claim.ID)
	return err
}
