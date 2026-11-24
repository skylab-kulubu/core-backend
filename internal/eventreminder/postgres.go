package eventreminder

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	// planLock is the advisory lock a pass takes while it plans: one
	// replica plans at a time, another one skips.
	planLock = 1836317471552853510
	// sendLock is the advisory lock a claim takes while it counts the
	// minute's sends and takes a row: the per-minute bound holds across
	// replicas.
	sendLock = 4415293882992736127
)

// PostgresQueue is the Queue in event_reminders and event_reminder_mails.
// Several cores share it: planning is one replica's at a time, a claim skips
// the row another holds.
type PostgresQueue struct {
	pool *pgxpool.Pool
}

func NewPostgresQueue(pool *pgxpool.Pool) *PostgresQueue {
	return &PostgresQueue{pool: pool}
}

// Plan plans the reminder of each published, current Event that starts
// within w and has had none for that Istanbul day: a row for each of its
// active Tickets, due at now. It answers how many Events and rows it
// planned; nothing when another replica is planning.
func (q *PostgresQueue) Plan(ctx context.Context, now time.Time, w Window) (int, int, error) {
	tx, err := q.pool.Begin(ctx)
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback(ctx)
	var locked bool
	if err := tx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock($1)`, int64(planLock)).Scan(&locked); err != nil || !locked {
		return 0, 0, err
	}
	rows, err := tx.Query(ctx, `
		SELECT e.id, e.start_date FROM events e
		WHERE e.status = 'published' AND e.archived_at IS NULL
		  AND e.start_date >= $1 AND e.start_date < $2 AND e.start_date > $3
		  AND NOT EXISTS (
			SELECT 1 FROM event_reminders r
			WHERE r.event_id = e.id AND r.starts_on = (e.start_date AT TIME ZONE '`+timeZone+`')::date)
		ORDER BY e.start_date, e.id`, w.From, w.To, w.After)
	if err != nil {
		return 0, 0, err
	}
	type due struct {
		id     uuid.UUID
		starts time.Time
	}
	events, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (due, error) {
		var d due
		err := row.Scan(&d.id, &d.starts)
		return d, err
	})
	if err != nil {
		return 0, 0, err
	}
	planned, queued := 0, 0
	for _, ev := range events {
		tag, err := tx.Exec(ctx, `
			INSERT INTO event_reminders (event_id, starts_on, starts_at, planned_at)
			VALUES ($1, ($2::timestamptz AT TIME ZONE '`+timeZone+`')::date, $2, $3)
			ON CONFLICT DO NOTHING`, ev.id, ev.starts, now)
		if err != nil {
			return 0, 0, err
		}
		if tag.RowsAffected() == 0 {
			continue
		}
		tag, err = tx.Exec(ctx, `
			INSERT INTO event_reminder_mails (id, event_id, ticket_id, starts_at, created_at, next_attempt_at)
			SELECT gen_random_uuid(), t.event_id, t.id, $2, $3, $3
			FROM tickets t WHERE t.event_id = $1 AND t.status = 'active'
			ON CONFLICT ON CONSTRAINT event_reminder_mails_key DO NOTHING`, ev.id, ev.starts, now)
		if err != nil {
			return 0, 0, err
		}
		if _, err := tx.Exec(ctx, `UPDATE event_reminders SET tickets = $3 WHERE event_id = $1 AND starts_at = $2`,
			ev.id, ev.starts, tag.RowsAffected()); err != nil {
			return 0, 0, err
		}
		planned++
		queued += int(tag.RowsAffected())
	}
	return planned, queued, tx.Commit(ctx)
}

const mailCols = `id, event_id, ticket_id, starts_at, created_at, next_attempt_at, attempts, claimed_at`

func scanMail(row pgx.Row) (Mail, error) {
	var m Mail
	var claimed *time.Time
	err := row.Scan(&m.ID, &m.EventID, &m.TicketID, &m.StartsAt, &m.CreatedAt, &m.NextAttemptAt, &m.Attempts, &claimed)
	if claimed != nil {
		m.ClaimedAt = *claimed
	}
	return m, err
}

// Claim takes the row due first and holds it for lease, unless perMinute
// rows (perMinute > 0) led a send in the minute before now: then limited
// is true and nothing is taken. The count and the claim happen under
// sendLock, so two replicas never both take the minute's last place.
func (q *PostgresQueue) Claim(ctx context.Context, now time.Time, lease time.Duration, perMinute int) (Mail, bool, bool, error) {
	tx, err := q.pool.Begin(ctx)
	if err != nil {
		return Mail{}, false, false, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, int64(sendLock)); err != nil {
		return Mail{}, false, false, err
	}
	if perMinute > 0 {
		var recent int
		if err := tx.QueryRow(ctx, `
			SELECT count(*) FROM event_reminder_mails
			WHERE attempted_at > $1::timestamptz - interval '60 seconds' AND attempted_at <= $1`, now).Scan(&recent); err != nil {
			return Mail{}, false, false, err
		}
		if recent >= perMinute {
			return Mail{}, false, true, nil
		}
	}
	m, err := scanMail(tx.QueryRow(ctx, `
		UPDATE event_reminder_mails SET claimed_at = $1, attempted_at = $1, next_attempt_at = $2
		WHERE id = (
			SELECT id FROM event_reminder_mails
			WHERE done_at IS NULL AND next_attempt_at <= $1
			ORDER BY next_attempt_at, created_at, id
			LIMIT 1
			FOR UPDATE SKIP LOCKED)
		RETURNING `+mailCols, now, now.Add(lease)))
	if errors.Is(err, pgx.ErrNoRows) {
		return Mail{}, false, false, nil
	}
	if err != nil {
		return Mail{}, false, false, err
	}
	return m, true, false, tx.Commit(ctx)
}

// Siblings are the other open rows of the claimed one's reminder (Event and
// start) whose Ticket is held under email, as stored: a guest Ticket under
// that address, or a registered one whose holder's primary e-mail it is. Not
// those another pass holds. The worker checks each again as it reads the
// holder.
func (q *PostgresQueue) Siblings(ctx context.Context, claimed Mail, email string, now time.Time) ([]Mail, error) {
	rows, err := q.pool.Query(ctx, `
		SELECT m.id, m.event_id, m.ticket_id, m.starts_at, m.created_at, m.next_attempt_at, m.attempts, m.claimed_at
		FROM event_reminder_mails m
		JOIN tickets t ON t.id = m.ticket_id
		LEFT JOIN users u ON u.id = t.owner_id AND t.ticket_type <> 'GUEST'
		WHERE m.done_at IS NULL AND m.id <> $1 AND m.event_id = $2 AND m.starts_at = $3
		  AND (m.claimed_at IS NULL OR m.next_attempt_at <= $5)
		  AND ((t.ticket_type = 'GUEST' AND lower(btrim(t.guest_email)) = $4) OR lower(btrim(u.email)) = $4)
		ORDER BY m.created_at, m.id`, claimed.ID, claimed.EventID, claimed.StartsAt, email, now)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Mail, error) { return scanMail(row) })
}

// ClaimSibling takes a sibling for the pass that holds the row it joins:
// false when another pass took it, or it was done with, meanwhile.
func (q *PostgresQueue) ClaimSibling(ctx context.Context, id uuid.UUID, now time.Time, lease time.Duration) (Mail, bool, error) {
	m, err := scanMail(q.pool.QueryRow(ctx, `
		UPDATE event_reminder_mails SET claimed_at = $2, next_attempt_at = $3
		WHERE id = $1 AND done_at IS NULL AND (claimed_at IS NULL OR next_attempt_at <= $2)
		RETURNING `+mailCols, id, now, now.Add(lease)))
	if errors.Is(err, pgx.ErrNoRows) {
		return Mail{}, false, nil
	}
	return m, err == nil, err
}

// Finish marks the claimed row done with outcome, while its pass still
// holds it. A row done with no mail (dropped, skipped, expired) gives its
// place in the minute back: a cancelled Event's rows do not hold up the
// others' mails.
func (q *PostgresQueue) Finish(ctx context.Context, claimed Mail, at time.Time, outcome Outcome) error {
	_, err := q.pool.Exec(ctx, `
		UPDATE event_reminder_mails SET done_at = $3, outcome = $4, claimed_at = NULL,
			attempted_at = CASE WHEN $4 IN ('dropped', 'skipped', 'expired') THEN NULL ELSE attempted_at END
		WHERE id = $1 AND claimed_at = $2`, claimed.ID, claimed.ClaimedAt, at, string(outcome))
	return err
}

// Retry counts a failure, makes the claimed row due at at and releases it.
func (q *PostgresQueue) Retry(ctx context.Context, claimed Mail, at time.Time) error {
	_, err := q.pool.Exec(ctx, `
		UPDATE event_reminder_mails SET attempts = attempts + 1, next_attempt_at = $3, claimed_at = NULL
		WHERE id = $1 AND claimed_at = $2`, claimed.ID, claimed.ClaimedAt, at)
	return err
}

var _ Queue = (*PostgresQueue)(nil)
