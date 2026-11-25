package doorstaffmail

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PostgresQueue is the Queue in door_staff_notifications. Several cores
// share it: a claim skips the row another holds.
type PostgresQueue struct {
	pool *pgxpool.Pool
}

func NewPostgresQueue(pool *pgxpool.Pool) *PostgresQueue {
	return &PostgresQueue{pool: pool}
}

func (q *PostgresQueue) Claim(ctx context.Context, now time.Time, lease time.Duration) (Queued, bool, error) {
	var c Queued
	err := q.pool.QueryRow(ctx, `
		UPDATE door_staff_notifications
		SET next_attempt_at = $2, claimed_at = $1
		WHERE id = (
			SELECT id FROM door_staff_notifications
			WHERE next_attempt_at <= $1
			ORDER BY next_attempt_at, created_at, id
			LIMIT 1
			FOR UPDATE SKIP LOCKED)
		RETURNING id, event_id, user_id, created_at, attempts, claimed_at`,
		now, now.Add(lease)).Scan(&c.ID, &c.EventID, &c.UserID, &c.CreatedAt, &c.Attempts, &c.ClaimedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Queued{}, false, nil
	}
	if err != nil {
		return Queued{}, false, err
	}
	return c, true, nil
}

func (q *PostgresQueue) Complete(ctx context.Context, claimed Queued) error {
	_, err := q.pool.Exec(ctx, `DELETE FROM door_staff_notifications WHERE id = $1 AND claimed_at = $2`, claimed.ID, claimed.ClaimedAt)
	return err
}

func (q *PostgresQueue) Retry(ctx context.Context, claimed Queued, at time.Time, failed bool) error {
	_, err := q.pool.Exec(ctx, `
		UPDATE door_staff_notifications
		SET attempts = attempts + CASE WHEN $4 THEN 1 ELSE 0 END, next_attempt_at = $3, claimed_at = NULL
		WHERE id = $1 AND claimed_at = $2`, claimed.ID, claimed.ClaimedAt, at, failed)
	return err
}

var _ Queue = (*PostgresQueue)(nil)
