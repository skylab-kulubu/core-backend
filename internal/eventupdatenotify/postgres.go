package eventupdatenotify

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PostgresQueue is the Queue in event_update_notifications and
// event_update_notification_sends. Several cores share it: a claim skips the
// row another holds. The Event's update writes the rows (event.PostgresStore
// with NotifyUpdated on).
type PostgresQueue struct {
	pool *pgxpool.Pool
}

func NewPostgresQueue(pool *pgxpool.Pool) *PostgresQueue {
	return &PostgresQueue{pool: pool}
}

// Claim marks the row started: from then on a change of the Event queues a
// row of its own, which waits until this one is done.
func (q *PostgresQueue) Claim(ctx context.Context, now time.Time, lease time.Duration) (Queued, bool, error) {
	var c Queued
	err := q.pool.QueryRow(ctx, `
		UPDATE event_update_notifications
		SET next_attempt_at = $2, claimed_at = $1, started_at = COALESCE(started_at, $1)
		WHERE id = (
			SELECT n.id FROM event_update_notifications n
			WHERE n.next_attempt_at <= $1
			  AND NOT EXISTS (
				SELECT 1 FROM event_update_notifications o
				WHERE o.event_id = n.event_id AND (o.created_at, o.id) < (n.created_at, n.id))
			ORDER BY n.next_attempt_at, n.created_at, n.id
			LIMIT 1
			FOR UPDATE SKIP LOCKED)
		RETURNING id, event_id, before_start_date, before_end_date, before_location, created_at, attempts, claimed_at`,
		now, now.Add(lease)).Scan(&c.ID, &c.EventID, &c.BeforeStart, &c.BeforeEnd, &c.BeforeLocation, &c.CreatedAt, &c.Attempts, &c.ClaimedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Queued{}, false, nil
	}
	if err != nil {
		return Queued{}, false, err
	}
	return c, true, nil
}

func (q *PostgresQueue) Renew(ctx context.Context, claimed Queued, until time.Time) error {
	_, err := q.pool.Exec(ctx, `UPDATE event_update_notifications SET next_attempt_at = $3 WHERE id = $1 AND claimed_at = $2`,
		claimed.ID, claimed.ClaimedAt, until)
	return err
}

func (q *PostgresQueue) Sends(ctx context.Context, id uuid.UUID) ([]uuid.UUID, error) {
	rows, err := q.pool.Query(ctx, `SELECT ticket_id FROM event_update_notification_sends WHERE notification_id = $1 ORDER BY sent_at, ticket_id`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []uuid.UUID{}
	for rows.Next() {
		var ticketID uuid.UUID
		if err := rows.Scan(&ticketID); err != nil {
			return nil, err
		}
		out = append(out, ticketID)
	}
	return out, rows.Err()
}

// RecordSend records nothing for a Ticket deleted since it was read: its
// record would go with it anyway.
func (q *PostgresQueue) RecordSend(ctx context.Context, id, ticketID uuid.UUID) error {
	_, err := q.pool.Exec(ctx, `
		INSERT INTO event_update_notification_sends (notification_id, ticket_id)
		SELECT $1, t.id FROM tickets t WHERE t.id = $2
		ON CONFLICT DO NOTHING`, id, ticketID)
	return err
}

func (q *PostgresQueue) Complete(ctx context.Context, claimed Queued) error {
	_, err := q.pool.Exec(ctx, `DELETE FROM event_update_notifications WHERE id = $1 AND claimed_at = $2`, claimed.ID, claimed.ClaimedAt)
	return err
}

func (q *PostgresQueue) Retry(ctx context.Context, claimed Queued, at time.Time) error {
	_, err := q.pool.Exec(ctx, `
		UPDATE event_update_notifications
		SET attempts = attempts + 1, next_attempt_at = $3, claimed_at = NULL
		WHERE id = $1 AND claimed_at = $2`, claimed.ID, claimed.ClaimedAt, at)
	return err
}

var _ Queue = (*PostgresQueue)(nil)
