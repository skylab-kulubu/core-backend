package media

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// MapClaim is a map worker's claim on one Event (migration 20261122160000):
// the point it draws, the claim's id and lease, and the tries that failed in
// a row before.
type MapClaim struct {
	EventID  uuid.UUID
	Lat, Lng float64
	ID       uuid.UUID
	Until    time.Time
	Attempts int
}

// ClaimNextMap claims, in one short transaction that commits before the map
// is drawn, the first current Event after the given id with a point and no
// map, not given up on, its wait over and no live claim on it. A row
// another transaction holds is skipped. found is false when there is none.
func (s *PostgresStore) ClaimNextMap(ctx context.Context, now time.Time, after uuid.UUID, lease time.Duration) (claim MapClaim, found bool, err error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return MapClaim{}, false, err
	}
	defer tx.Rollback(ctx)
	// The conditions start with events_map_due_idx's predicate.
	err = tx.QueryRow(ctx, `SELECT id, location_lat, location_lng, map_attempts FROM events
		WHERE map_media_id IS NULL AND map_state IS NULL AND location_lat IS NOT NULL AND archived_at IS NULL
		  AND id > $2
		  AND (map_retry_at IS NULL OR map_retry_at <= $1)
		  AND (map_claimed_until IS NULL OR map_claimed_until <= $1)
		ORDER BY id LIMIT 1
		FOR NO KEY UPDATE SKIP LOCKED`, now, after).Scan(&claim.EventID, &claim.Lat, &claim.Lng, &claim.Attempts)
	if errors.Is(err, pgx.ErrNoRows) {
		return MapClaim{}, false, nil
	}
	if err != nil {
		return MapClaim{}, false, err
	}
	claim.ID, claim.Until = uuid.New(), now.Add(lease)
	if _, err := tx.Exec(ctx, `UPDATE events SET map_claim_id = $2, map_claimed_until = $3 WHERE id = $1`,
		claim.EventID, claim.ID, claim.Until); err != nil {
		return MapClaim{}, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return MapClaim{}, false, err
	}
	return claim, true, nil
}

// CreateMapRecord stores the record of a map whose object is about to be
// written: pending, with its purpose's expiry, and no uploader.
func (s *PostgresStore) CreateMapRecord(ctx context.Context, m Media) (Media, error) {
	m.UploadedBy = uuid.Nil
	return insertMedia(ctx, s.pool, newRecord(m))
}

// AttachMap links the map to the claimed Event and lets go of the claim, in
// one statement whose trigger writes the map's Media attachment. attached
// is false, and nothing changes, when the claim is no longer this one, the
// Event has another point or a map already, or it was archived.
func (s *PostgresStore) AttachMap(ctx context.Context, claim MapClaim, mapID uuid.UUID) (attached bool, err error) {
	tag, err := s.pool.Exec(ctx, `UPDATE events SET
			map_media_id = $5, map_attempts = 0, map_retry_at = NULL,
			map_claim_id = NULL, map_claimed_until = NULL
		WHERE id = $1 AND map_claim_id = $2 AND map_media_id IS NULL AND archived_at IS NULL
		  AND location_lat = $3 AND location_lng = $4`,
		claim.EventID, claim.ID, claim.Lat, claim.Lng, mapID)
	if refusal, ok := DatabaseLinkRefusal(err); ok {
		return false, refusal
	}
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// DeferMap puts off an Event whose map failed and lets go of its claim:
// each failure in a row doubles the wait, from mapRetryFirst up to
// mapRetryMax. giveUp fails it instead. A claim no longer this one changes
// nothing.
func (s *PostgresStore) DeferMap(ctx context.Context, claim MapClaim, now time.Time, giveUp bool) error {
	_, err := s.pool.Exec(ctx, `UPDATE events SET
			map_state = CASE WHEN $4 THEN 'failed' ELSE map_state END,
			map_retry_at = CASE WHEN $4 THEN NULL
				ELSE $3::TIMESTAMPTZ + LEAST($5::INTERVAL * power(2, LEAST(map_attempts, 20)), $6::INTERVAL) END,
			map_attempts = map_attempts + 1,
			map_claim_id = NULL, map_claimed_until = NULL
		WHERE id = $1 AND map_claim_id = $2`,
		claim.EventID, claim.ID, now, giveUp, mapRetryFirst, mapRetryMax)
	return err
}

// FailMap gives up on the Event's point at once, counting the try: the
// source has no map of it, or the step panicked. A new point starts over.
func (s *PostgresStore) FailMap(ctx context.Context, claim MapClaim) error {
	_, err := s.pool.Exec(ctx, `UPDATE events SET
			map_state = 'failed', map_attempts = map_attempts + 1, map_retry_at = NULL,
			map_claim_id = NULL, map_claimed_until = NULL
		WHERE id = $1 AND map_claim_id = $2`, claim.EventID, claim.ID)
	return err
}

// ReleaseMap lets go of a claim, counting nothing against the Event.
func (s *PostgresStore) ReleaseMap(ctx context.Context, claim MapClaim) error {
	_, err := s.pool.Exec(ctx, `UPDATE events SET map_claim_id = NULL, map_claimed_until = NULL
		WHERE id = $1 AND map_claim_id = $2`, claim.EventID, claim.ID)
	return err
}
