package attendancemail

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// sendLock is the advisory lock a claim takes while it counts the minute's
// mails and takes a row: the per-minute bound holds across replicas.
const sendLock = 6202542410935577117

// PostgresQueue is the Queue in attendance_mails. Several cores share it: a
// claim skips the row another holds. The rows are queued by the ticket
// store (ticket.PostgresStore.NotifyAttendance).
type PostgresQueue struct {
	pool *pgxpool.Pool
}

func NewPostgresQueue(pool *pgxpool.Pool) *PostgresQueue {
	return &PostgresQueue{pool: pool}
}

const mailCols = `id, ticket_id, event_id, kind, entry_date::text, session_id, created_at, next_attempt_at, attempts, claimed_at, COALESCE(mail_key, '')`

func scanMail(row pgx.Row) (Mail, error) {
	var m Mail
	var claimed *time.Time
	err := row.Scan(&m.ID, &m.TicketID, &m.EventID, &m.Kind, &m.EntryDate, &m.SessionID, &m.CreatedAt, &m.NextAttemptAt, &m.Attempts, &claimed, &m.MailKey)
	if claimed != nil {
		m.ClaimedAt = *claimed
	}
	return m, err
}

// Claim takes the row due first, a welcome before a receipt, and holds it
// for lease, unless perMinute rows (perMinute > 0) led a send in the minute
// before now: then limited is true and nothing is taken. The count and the
// claim happen under sendLock, so two replicas never both take the minute's
// last place.
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
			SELECT count(*) FROM attendance_mails
			WHERE attempted_at > $1::timestamptz - interval '60 seconds' AND attempted_at <= $1`, now).Scan(&recent); err != nil {
			return Mail{}, false, false, err
		}
		if recent >= perMinute {
			return Mail{}, false, true, nil
		}
	}
	m, err := scanMail(tx.QueryRow(ctx, `
		UPDATE attendance_mails SET claimed_at = $1, attempted_at = $1, next_attempt_at = $2
		WHERE id = (
			SELECT id FROM attendance_mails
			WHERE done_at IS NULL AND next_attempt_at <= $1
			ORDER BY (kind = 'welcome') DESC, next_attempt_at, created_at, id
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

// Siblings are the open receipts of the claimed row's Ticket and day that
// go along: due or not for a welcome, due for a receipt. Not those another
// pass holds.
func (q *PostgresQueue) Siblings(ctx context.Context, claimed Mail, now time.Time) ([]Mail, error) {
	rows, err := q.pool.Query(ctx, `
		SELECT `+mailCols+`
		FROM attendance_mails
		WHERE done_at IS NULL AND id <> $1 AND ticket_id = $2 AND entry_date = $3::date AND kind = 'recorded'
		  AND (claimed_at IS NULL OR next_attempt_at <= $4)
		  AND ($5 OR next_attempt_at <= $4)
		  AND COALESCE(mail_key, '') = $6
		ORDER BY created_at, id`, claimed.ID, claimed.TicketID, claimed.EntryDate, now, claimed.Kind == KindWelcome, claimed.MailKey)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Mail, error) { return scanMail(row) })
}

// ClaimSibling takes a sibling for the pass that holds the row it joins:
// false when another pass took it, or it was done with, meanwhile.
func (q *PostgresQueue) ClaimSibling(ctx context.Context, id uuid.UUID, now time.Time, lease time.Duration) (Mail, bool, error) {
	m, err := scanMail(q.pool.QueryRow(ctx, `
		UPDATE attendance_mails SET claimed_at = $2, next_attempt_at = $3
		WHERE id = $1 AND done_at IS NULL AND (claimed_at IS NULL OR next_attempt_at <= $2)
		RETURNING `+mailCols, id, now, now.Add(lease)))
	if errors.Is(err, pgx.ErrNoRows) {
		return Mail{}, false, nil
	}
	return m, err == nil, err
}

// Present reports whether the row's Venue entry (a welcome) or Check-in (a
// receipt) is still there.
func (q *PostgresQueue) Present(ctx context.Context, m Mail) (bool, error) {
	var present bool
	var err error
	if m.Kind == KindWelcome {
		err = q.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM venue_entries WHERE ticket_id = $1 AND entry_date = $2::date)`,
			m.TicketID, m.EntryDate).Scan(&present)
	} else {
		// The Session's receipt is no news once the Session is cancelled or
		// archived (or its day is).
		err = q.pool.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM ticket_checkins c
				JOIN sessions s ON s.id = c.session_id AND NOT s.cancelled AND s.archived_at IS NULL
				JOIN event_days d ON d.id = s.event_day_id AND d.archived_at IS NULL
				WHERE c.ticket_id = $1 AND c.session_id = $2)`,
			m.TicketID, m.SessionID).Scan(&present)
	}
	return present, err
}

// Progress reads the Event's current days, its live Sessions (current day,
// not archived, not cancelled; the certificate rule's), the Ticket's
// Check-ins and the titles of sessions, in four small queries.
func (q *PostgresQueue) Progress(ctx context.Context, eventID, ticketID uuid.UUID, sessions []uuid.UUID) (Progress, error) {
	out := Progress{Attended: map[uuid.UUID]bool{}, Titles: map[uuid.UUID]string{}}
	rows, err := q.pool.Query(ctx, `
		SELECT id, start_date FROM event_days
		WHERE event_id = $1 AND archived_at IS NULL
		ORDER BY start_date NULLS LAST, id`, eventID)
	if err != nil {
		return Progress{}, err
	}
	out.Days, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (Day, error) {
		var d Day
		return d, row.Scan(&d.ID, &d.Start)
	})
	if err != nil {
		return Progress{}, err
	}
	rows, err = q.pool.Query(ctx, `
		SELECT s.id, s.title, d.id, s.start_time, s.end_time,
		       COALESCE(s.end_time, d.end_date, e.end_date, e.start_date + interval '24 hours'),
		       s.certificate_counted, s.certificate_required
		FROM sessions s
		JOIN event_days d ON d.id = s.event_day_id
		JOIN events e ON e.id = d.event_id
		WHERE d.event_id = $1 AND d.archived_at IS NULL AND s.archived_at IS NULL AND NOT s.cancelled
		ORDER BY d.start_date NULLS LAST, s.order_index, s.start_time NULLS LAST, s.id`, eventID)
	if err != nil {
		return Progress{}, err
	}
	out.Sessions, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (Session, error) {
		var s Session
		return s, row.Scan(&s.ID, &s.Title, &s.DayID, &s.Start, &s.End, &s.Ends, &s.Counted, &s.Required)
	})
	if err != nil {
		return Progress{}, err
	}
	rows, err = q.pool.Query(ctx, `SELECT DISTINCT session_id FROM ticket_checkins WHERE ticket_id = $1`, ticketID)
	if err != nil {
		return Progress{}, err
	}
	attended, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return Progress{}, err
	}
	for _, id := range attended {
		out.Attended[id] = true
	}
	if len(sessions) == 0 {
		return out, nil
	}
	rows, err = q.pool.Query(ctx, `SELECT id, title FROM sessions WHERE id = ANY($1)`, sessions)
	if err != nil {
		return Progress{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var id uuid.UUID
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			return Progress{}, err
		}
		out.Titles[id] = name
	}
	return out, rows.Err()
}

// Finish marks the claimed row done with outcome, while its pass still
// holds it. A row done with no mail (dropped, skipped, expired) gives its
// place in the minute back.
func (q *PostgresQueue) Finish(ctx context.Context, claimed Mail, at time.Time, outcome Outcome) error {
	_, err := q.pool.Exec(ctx, `
		UPDATE attendance_mails SET done_at = $3, outcome = $4, claimed_at = NULL,
			attempted_at = CASE WHEN $4 IN ('dropped', 'skipped', 'expired') THEN NULL ELSE attempted_at END
		WHERE id = $1 AND claimed_at = $2`, claimed.ID, claimed.ClaimedAt, at, string(outcome))
	return err
}

// Keyed writes key on the rows that have none, before their first try.
func (q *PostgresQueue) Keyed(ctx context.Context, rows []Mail, key string) error {
	ids := make([]uuid.UUID, 0, len(rows))
	for _, m := range rows {
		ids = append(ids, m.ID)
	}
	_, err := q.pool.Exec(ctx, `UPDATE attendance_mails SET mail_key = $2 WHERE id = ANY($1) AND mail_key IS NULL`, ids, key)
	return err
}

// Attempted counts m's mail in the minute at at.
func (q *PostgresQueue) Attempted(ctx context.Context, m Mail, at time.Time) error {
	_, err := q.pool.Exec(ctx, `UPDATE attendance_mails SET attempted_at = $2 WHERE id = $1`, m.ID, at)
	return err
}

// Retry counts a failure, makes the claimed row due at at and releases it.
func (q *PostgresQueue) Retry(ctx context.Context, claimed Mail, at time.Time) error {
	_, err := q.pool.Exec(ctx, `
		UPDATE attendance_mails SET attempts = attempts + 1, next_attempt_at = $3, claimed_at = NULL
		WHERE id = $1 AND claimed_at = $2`, claimed.ID, claimed.ClaimedAt, at)
	return err
}

// Purge removes the rows done before before.
func (q *PostgresQueue) Purge(ctx context.Context, before time.Time) (int, error) {
	tag, err := q.pool.Exec(ctx, `DELETE FROM attendance_mails WHERE done_at IS NOT NULL AND done_at < $1`, before)
	return int(tag.RowsAffected()), err
}

var _ Queue = (*PostgresQueue)(nil)
