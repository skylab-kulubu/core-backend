package ticketnotify

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// PostgresQueue is the Queue in ticket_notifications. Several cores share
// it: a claim skips the rows another holds.
type PostgresQueue struct {
	pool *pgxpool.Pool
}

func NewPostgresQueue(pool *pgxpool.Pool) *PostgresQueue {
	return &PostgresQueue{pool: pool}
}

func (q *PostgresQueue) Claim(ctx context.Context, now time.Time, lease time.Duration, limit int) ([]Queued, error) {
	rows, err := q.pool.Query(ctx, `
		UPDATE ticket_notifications n
		SET next_attempt_at = $2, claimed_at = $1
		FROM (
			SELECT ticket_id FROM ticket_notifications
			WHERE next_attempt_at <= $1
			ORDER BY next_attempt_at, ticket_id
			LIMIT $3
			FOR UPDATE SKIP LOCKED
		) due
		WHERE n.ticket_id = due.ticket_id
		RETURNING n.ticket_id, n.created_at, n.attempts, n.claimed_at`,
		now, now.Add(lease), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Queued
	for rows.Next() {
		var c Queued
		if err := rows.Scan(&c.TicketID, &c.CreatedAt, &c.Attempts, &c.ClaimedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (q *PostgresQueue) Complete(ctx context.Context, claimed Queued) error {
	_, err := q.pool.Exec(ctx, `DELETE FROM ticket_notifications WHERE ticket_id = $1 AND claimed_at = $2`, claimed.TicketID, claimed.ClaimedAt)
	return err
}

func (q *PostgresQueue) Retry(ctx context.Context, claimed Queued, at time.Time) error {
	_, err := q.pool.Exec(ctx, `
		UPDATE ticket_notifications
		SET attempts = attempts + 1, next_attempt_at = $3, claimed_at = NULL
		WHERE ticket_id = $1 AND claimed_at = $2`, claimed.TicketID, claimed.ClaimedAt, at)
	return err
}

var _ Queue = (*PostgresQueue)(nil)
