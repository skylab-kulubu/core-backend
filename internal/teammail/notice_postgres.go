package teammail

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PostgresNoticeQueue keeps the privilege notices in privilege_notice_mails.
type PostgresNoticeQueue struct {
	pool *pgxpool.Pool
}

func NewPostgresNoticeQueue(pool *pgxpool.Pool) *PostgresNoticeQueue {
	return &PostgresNoticeQueue{pool: pool}
}

func orEmpty(names []string) []string {
	if names == nil {
		return []string{}
	}
	return names
}

func (q *PostgresNoticeQueue) Enqueue(ctx context.Context, n Notice) error {
	_, err := q.pool.Exec(ctx, `
		INSERT INTO privilege_notice_mails
			(id, change_id, event, group_path, subject_id, actor_id, roles_added, roles_removed, occurred_at, next_attempt_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $9)
		ON CONFLICT DO NOTHING`,
		n.ID, n.ChangeID, string(n.Event), n.GroupPath, n.SubjectID, n.ActorID, orEmpty(n.RolesAdded), orEmpty(n.RolesRemoved), n.OccurredAt)
	return err
}

func (q *PostgresNoticeQueue) Claim(ctx context.Context, now time.Time, lease time.Duration, limit int) ([]Notice, error) {
	rows, err := q.pool.Query(ctx, `
		UPDATE privilege_notice_mails SET next_attempt_at = $2, claimed_at = $1
		WHERE id IN (
			SELECT id FROM privilege_notice_mails
			WHERE next_attempt_at <= $1
			ORDER BY next_attempt_at, occurred_at, id
			LIMIT $3
			FOR UPDATE SKIP LOCKED)
		RETURNING id, change_id, recipient_id, event, group_path, subject_id, actor_id, roles_added, roles_removed,
			member_count, occurred_at, next_attempt_at, attempts, claimed_at`,
		now, now.Add(lease), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Notice
	for rows.Next() {
		var n Notice
		var event string
		var count *int32
		if err := rows.Scan(&n.ID, &n.ChangeID, &n.RecipientID, &event, &n.GroupPath, &n.SubjectID, &n.ActorID,
			&n.RolesAdded, &n.RolesRemoved, &count, &n.OccurredAt, &n.NextAttemptAt, &n.Attempts, &n.ClaimedAt); err != nil {
			return nil, err
		}
		n.Event = NoticeEvent(event)
		if count != nil {
			c := int(*count)
			n.MemberCount = &c
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

func (q *PostgresNoticeQueue) Expand(ctx context.Context, claimed Notice, recipients []uuid.UUID, memberCount *int, at time.Time) (int, error) {
	// One statement: the change row goes, fenced by its claim, and one row
	// per recipient takes its place. A claim taken since deletes nothing,
	// and so inserts nothing.
	tag, err := q.pool.Exec(ctx, `
		WITH change AS (
			DELETE FROM privilege_notice_mails
			WHERE id = $1 AND claimed_at = $2 AND recipient_id IS NULL
			RETURNING *
		)
		INSERT INTO privilege_notice_mails
			(id, change_id, recipient_id, event, group_path, subject_id, actor_id, roles_added, roles_removed,
			 member_count, occurred_at, next_attempt_at)
		SELECT gen_random_uuid(), c.change_id, r.recipient, c.event, c.group_path, c.subject_id, c.actor_id,
			c.roles_added, c.roles_removed, $4, c.occurred_at, $5
		FROM change c CROSS JOIN unnest($3::uuid[]) AS r(recipient)
		ON CONFLICT DO NOTHING`,
		claimed.ID, claimed.ClaimedAt, recipients, memberCount, at)
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}

func (q *PostgresNoticeQueue) Complete(ctx context.Context, claimed Notice) error {
	_, err := q.pool.Exec(ctx, `DELETE FROM privilege_notice_mails WHERE id = $1 AND claimed_at = $2`, claimed.ID, claimed.ClaimedAt)
	return err
}

func (q *PostgresNoticeQueue) Retry(ctx context.Context, claimed Notice, at time.Time) error {
	_, err := q.pool.Exec(ctx, `
		UPDATE privilege_notice_mails SET attempts = attempts + 1, next_attempt_at = $3, claimed_at = NULL
		WHERE id = $1 AND claimed_at = $2`, claimed.ID, claimed.ClaimedAt, at)
	return err
}

func (q *PostgresNoticeQueue) DropBefore(ctx context.Context, occurredBefore time.Time) (int, error) {
	tag, err := q.pool.Exec(ctx, `DELETE FROM privilege_notice_mails WHERE occurred_at < $1`, occurredBefore)
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}

func (q *PostgresNoticeQueue) Backlog(ctx context.Context, now time.Time) (int, time.Duration, error) {
	var count int
	var oldest *time.Time
	if err := q.pool.QueryRow(ctx, `SELECT count(*), min(occurred_at) FROM privilege_notice_mails`).Scan(&count, &oldest); err != nil {
		return 0, 0, err
	}
	if oldest == nil {
		return 0, 0, nil
	}
	return count, max(now.Sub(*oldest), 0), nil
}

var _ NoticeQueue = (*PostgresNoticeQueue)(nil)
