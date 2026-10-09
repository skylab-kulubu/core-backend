package teammail

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PostgresQueue keeps the queue in team_membership_mails.
type PostgresQueue struct {
	pool *pgxpool.Pool
}

func NewPostgresQueue(pool *pgxpool.Pool) *PostgresQueue { return &PostgresQueue{pool: pool} }

func (q *PostgresQueue) Enqueue(ctx context.Context, c Change) error {
	_, err := q.pool.Exec(ctx, `
		INSERT INTO team_membership_mails (id, subject_id, actor_id, group_path, action, occurred_at, next_attempt_at)
		VALUES ($1, $2, $3, $4, $5, $6, $6)`,
		c.ID, c.SubjectID, c.ActorID, c.GroupPath, string(c.Action), c.OccurredAt)
	return err
}

func (q *PostgresQueue) Claim(ctx context.Context, now time.Time, lease time.Duration, limit int) ([]Change, error) {
	rows, err := q.pool.Query(ctx, `
		UPDATE team_membership_mails SET next_attempt_at = $2
		WHERE id IN (
			SELECT id FROM team_membership_mails
			WHERE next_attempt_at <= $1
			ORDER BY next_attempt_at, occurred_at, id
			LIMIT $3
			FOR UPDATE SKIP LOCKED)
		RETURNING id, subject_id, actor_id, group_path, action, occurred_at, next_attempt_at, attempts`,
		now, now.Add(lease), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Change
	for rows.Next() {
		var c Change
		var action string
		if err := rows.Scan(&c.ID, &c.SubjectID, &c.ActorID, &c.GroupPath, &action, &c.OccurredAt, &c.NextAttemptAt, &c.Attempts); err != nil {
			return nil, err
		}
		c.Action = Action(action)
		out = append(out, c)
	}
	return out, rows.Err()
}

func (q *PostgresQueue) Complete(ctx context.Context, id uuid.UUID) error {
	_, err := q.pool.Exec(ctx, `DELETE FROM team_membership_mails WHERE id = $1`, id)
	return err
}

func (q *PostgresQueue) Retry(ctx context.Context, id uuid.UUID, at time.Time) error {
	_, err := q.pool.Exec(ctx, `UPDATE team_membership_mails SET attempts = attempts + 1, next_attempt_at = $2 WHERE id = $1`, id, at)
	return err
}

func (q *PostgresQueue) DropBefore(ctx context.Context, occurredBefore time.Time) (int, error) {
	tag, err := q.pool.Exec(ctx, `DELETE FROM team_membership_mails WHERE occurred_at < $1`, occurredBefore)
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}

func (q *PostgresQueue) Backlog(ctx context.Context, now time.Time) (int, time.Duration, error) {
	var count int
	var oldest *time.Time
	if err := q.pool.QueryRow(ctx, `SELECT count(*), min(occurred_at) FROM team_membership_mails`).Scan(&count, &oldest); err != nil {
		return 0, 0, err
	}
	if oldest == nil {
		return 0, 0, nil
	}
	return count, max(now.Sub(*oldest), 0), nil
}

var _ Queue = (*PostgresQueue)(nil)
