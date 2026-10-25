package feedback

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// The feedback mail (ticket 06, docs/event-feedback.md): which targets are
// due, the queue of mails, one per Ticket, and the targets each carries.

const (
	// SendDelay is how long after a target ends its form is mailed (K3).
	SendDelay = 2 * time.Hour
	// AutoWindow bounds the schedule: a target that ended longer ago than
	// this is not mailed by itself (a form added to an old Event does not
	// mail its people out of the blue; "Şimdi gönder" still does).
	AutoWindow = 7 * 24 * time.Hour
	// dayGrace holds a person's mail of a day a little past its last
	// target's due time, so that the targets of the day that fall due at
	// that same minute join it instead of a mail of their own.
	dayGrace = 5 * time.Minute
	// planLock is the advisory lock the schedule's pass takes: one replica
	// plans at a time.
	planLock = 7206119531226841860
)

var (
	// ErrNoForm refuses "Şimdi gönder" on a target without a feedback form.
	ErrNoForm = errors.New("feedback: no form")
	// ErrNotSendable refuses "Şimdi gönder" on a draft, a cancelled or an
	// archived target.
	ErrNotSendable = errors.New("feedback: not sendable")
)

// Target is an Event (SessionID nil) or one of its Sessions.
type Target struct {
	EventID   uuid.UUID
	SessionID *uuid.UUID
}

// Mail is one queued feedback mail of a Ticket.
type Mail struct {
	ID       uuid.UUID
	EventID  uuid.UUID
	TicketID uuid.UUID
	// Kind is KindRequest, the form's mail, or KindReminder (ticket 07).
	Kind string
	// EventDayID is the day the schedule's mail gathers (nil for a manual
	// mail, or the Event's form of an Event without a day).
	EventDayID    *uuid.UUID
	Manual        bool
	CreatedAt     time.Time
	NextAttemptAt time.Time
	Attempts      int
	// ClaimedAt is the claim that returned the mail: Finish and Retry write
	// only while the row still carries it.
	ClaimedAt time.Time
}

// The kinds of feedback mail.
const (
	KindRequest  = "request"
	KindReminder = "reminder"
)

// MailItem is one target a mail carries, as it is now: what the worker
// decides with whether the target still goes.
type MailItem struct {
	Target
	// FormID is the form the target was sent with (feedback_mail_targets).
	FormID uuid.UUID
	// Event and Session, read now.
	EventName, EventStatus string
	EventArchived          bool
	FeedbackMode           string
	SessionTitle           string
	SessionGone            bool
	// FormURL and AutoSend are the target's form and box now.
	FormURL  string
	AutoSend bool
}

// dueTarget is a target the schedule found due.
type dueTarget struct {
	Target
	FormURL string
	EndsAt  time.Time
}

// eventEndsSQL is an Event's end (bilet 02's expression): its end date,
// else a day after its start; NULL when it has neither.
const eventEndsSQL = `COALESCE(e.end_date, e.start_date + interval '24 hours')`

// sessionEndsSQL is a Session's end: its end time, else its day's end,
// else its Event's.
const sessionEndsSQL = `COALESCE(s.end_time, d.end_date, ` + eventEndsSQL + `)`

// Plan queues the mails of the targets that are due at now (ended at least
// SendDelay ago, within AutoWindow, with a form and the box on, the Event
// published and current, a Session not cancelled, not sent yet). Each
// target is recorded as sent, once; each Ticket that checked in to it gets
// it in their mail of its EventDay, which goes SendDelay after the last
// Session they checked in to that day ends (S2). One replica plans at a
// time; another one skips. Answers how many targets it queued.
func (s *PostgresStore) Plan(ctx context.Context, now time.Time) (int, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	var locked bool
	if err := tx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock($1)`, int64(planLock)).Scan(&locked); err != nil || !locked {
		return 0, err
	}
	rows, err := tx.Query(ctx, `
		SELECT e.id, NULL::uuid, e.feedback_form_url, `+eventEndsSQL+`
		FROM events e
		WHERE e.archived_at IS NULL AND e.status = 'published'
		  AND e.feedback_form_url <> '' AND e.feedback_auto_send
		  AND `+eventEndsSQL+` + $2::float8 * interval '1 second' <= $1 AND `+eventEndsSQL+` > $1 - $3::float8 * interval '1 second'
		  AND NOT EXISTS (SELECT 1 FROM feedback_mail_targets t WHERE t.event_id = e.id AND t.session_id IS NULL)
		UNION ALL
		SELECT e.id, s.id, s.feedback_form_url, `+sessionEndsSQL+`
		FROM events e
		JOIN event_days d ON d.event_id = e.id AND d.archived_at IS NULL
		JOIN sessions s ON s.event_day_id = d.id AND s.archived_at IS NULL AND NOT s.cancelled
		WHERE e.archived_at IS NULL AND e.status = 'published' AND e.feedback_mode = 'per_session'
		  AND s.feedback_form_url <> '' AND s.feedback_auto_send
		  AND `+sessionEndsSQL+` + $2::float8 * interval '1 second' <= $1 AND `+sessionEndsSQL+` > $1 - $3::float8 * interval '1 second'
		  AND NOT EXISTS (SELECT 1 FROM feedback_mail_targets t WHERE t.event_id = e.id AND t.session_id = s.id)`,
		now, SendDelay.Seconds(), AutoWindow.Seconds())
	if err != nil {
		return 0, err
	}
	due, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (dueTarget, error) {
		var t dueTarget
		err := row.Scan(&t.EventID, &t.SessionID, &t.FormURL, &t.EndsAt)
		return t, err
	})
	if err != nil {
		return 0, err
	}
	queued := 0
	for _, t := range due {
		formID, ok := formIDFromURL(t.FormURL)
		if !ok {
			continue
		}
		taken, err := takeTarget(ctx, tx, t.Target, formID, "auto")
		if err != nil {
			return 0, err
		}
		if !taken {
			continue
		}
		if err := queueAuto(ctx, tx, t, now); err != nil {
			return 0, err
		}
		queued++
	}
	return queued, tx.Commit(ctx)
}

// takeTarget records the target as sent: false when it was already.
func takeTarget(ctx context.Context, tx pgx.Tx, t Target, formID uuid.UUID, trigger string) (bool, error) {
	tag, err := tx.Exec(ctx, `
		INSERT INTO feedback_mail_targets (event_id, session_id, form_id, trigger) VALUES ($1, $2, $3, $4)
		ON CONFLICT ON CONSTRAINT feedback_mail_targets_key DO NOTHING`, t.EventID, t.SessionID, formID, trigger)
	return tag.RowsAffected() == 1, err
}

// audienceSQL are the target's Tickets that checked in: to the Session, or
// to any Session of the Event ($2 NULL). Guests too (K2).
const audienceSQL = `
	SELECT DISTINCT c.ticket_id FROM ticket_checkins c
	JOIN sessions s ON s.id = c.session_id
	JOIN event_days d ON d.id = s.event_day_id
	WHERE d.event_id = $1 AND ($2::uuid IS NULL OR s.id = $2)`

func audience(ctx context.Context, tx pgx.Tx, t Target) ([]uuid.UUID, error) {
	rows, err := tx.Query(ctx, audienceSQL, t.EventID, t.SessionID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
}

// queueAuto puts the due target in the mail of its EventDay of each Ticket
// that checked in to it: the Session's day, or, for the Event's own form,
// its last current day (none when it has no day).
func queueAuto(ctx context.Context, tx pgx.Tx, t dueTarget, now time.Time) error {
	var day *uuid.UUID
	if t.SessionID != nil {
		if err := tx.QueryRow(ctx, `SELECT event_day_id FROM sessions WHERE id = $1`, *t.SessionID).Scan(&day); err != nil {
			return err
		}
	} else {
		err := tx.QueryRow(ctx, `
			SELECT id FROM event_days WHERE event_id = $1 AND archived_at IS NULL
			ORDER BY start_date DESC NULLS LAST, created_at DESC, id DESC LIMIT 1`, t.EventID).Scan(&day)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
	}
	tickets, err := audience(ctx, tx, t.Target)
	if err != nil {
		return err
	}
	for _, ticketID := range tickets {
		due, err := personDayDue(ctx, tx, ticketID, day, t.EndsAt)
		if err != nil {
			return err
		}
		if due.Before(now) {
			due = now
		}
		var mailID uuid.UUID
		err = tx.QueryRow(ctx, `
			SELECT id FROM feedback_mails
			WHERE ticket_id = $1 AND event_day_id IS NOT DISTINCT FROM $2 AND kind = 'request' AND NOT manual
			  AND done_at IS NULL AND claimed_at IS NULL
			ORDER BY created_at LIMIT 1 FOR UPDATE`, ticketID, day).Scan(&mailID)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			mailID = uuid.New()
			if _, err := tx.Exec(ctx, `
				INSERT INTO feedback_mails (id, event_id, ticket_id, event_day_id, next_attempt_at, created_at)
				VALUES ($1, $2, $3, $4, $5, $6)`, mailID, t.EventID, ticketID, day, due, now); err != nil {
				return err
			}
		case err != nil:
			return err
		default:
			if _, err := tx.Exec(ctx, `UPDATE feedback_mails SET next_attempt_at = GREATEST(next_attempt_at, $2) WHERE id = $1`, mailID, due); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO feedback_mail_items (mail_id, event_id, session_id) VALUES ($1, $2, $3)
			ON CONFLICT ON CONSTRAINT feedback_mail_items_key DO NOTHING`, mailID, t.EventID, t.SessionID); err != nil {
			return err
		}
	}
	return nil
}

// personDayDue is when the Ticket's mail of the day goes: SendDelay after
// the last of the day's current Sessions it checked in to ends, or after
// the target's own end when that is later, plus dayGrace.
func personDayDue(ctx context.Context, tx pgx.Tx, ticketID uuid.UUID, day *uuid.UUID, targetEnds time.Time) (time.Time, error) {
	last := targetEnds
	if day != nil {
		var latest *time.Time
		err := tx.QueryRow(ctx, `
			SELECT max(`+sessionEndsSQL+`)
			FROM ticket_checkins c
			JOIN sessions s ON s.id = c.session_id AND s.archived_at IS NULL AND NOT s.cancelled
			JOIN event_days d ON d.id = s.event_day_id
			JOIN events e ON e.id = d.event_id
			WHERE c.ticket_id = $1 AND d.id = $2`, ticketID, *day).Scan(&latest)
		if err != nil {
			return time.Time{}, err
		}
		if latest != nil && latest.After(last) {
			last = *latest
		}
	}
	return last.Add(SendDelay + dayGrace), nil
}

// SendNow mails the target's form at once ("Şimdi gönder"): a mail of its
// own to each Ticket that checked in, not waiting for the day's others.
// The target counts as sent: the schedule never sends it again. Sending it
// again is a no-op (false). ErrNoForm when it has no form, ErrNotSendable
// when the Event is not published (a draft, cancelled) or is archived, or
// the Session is cancelled or archived.
func (s *PostgresStore) SendNow(ctx context.Context, t Target, now time.Time) (bool, int, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, 0, err
	}
	defer tx.Rollback(ctx)
	var status, form string
	var archived bool
	if t.SessionID == nil {
		err = tx.QueryRow(ctx, `SELECT status, archived_at IS NOT NULL, feedback_form_url FROM events WHERE id = $1 FOR SHARE`, t.EventID).Scan(&status, &archived, &form)
	} else {
		err = tx.QueryRow(ctx, `
			SELECT e.status, e.archived_at IS NOT NULL OR d.archived_at IS NOT NULL OR s.archived_at IS NOT NULL OR s.cancelled, s.feedback_form_url
			FROM sessions s JOIN event_days d ON d.id = s.event_day_id JOIN events e ON e.id = d.event_id
			WHERE s.id = $1 AND e.id = $2 FOR SHARE OF e`, *t.SessionID, t.EventID).Scan(&status, &archived, &form)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return false, 0, ErrNotSendable
	}
	if err != nil {
		return false, 0, err
	}
	if archived || status != "published" {
		return false, 0, ErrNotSendable
	}
	formID, ok := formIDFromURL(form)
	if !ok {
		return false, 0, ErrNoForm
	}
	taken, err := takeTarget(ctx, tx, t, formID, "manual")
	if err != nil || !taken {
		return false, 0, err
	}
	tickets, err := audience(ctx, tx, t)
	if err != nil {
		return false, 0, err
	}
	for _, ticketID := range tickets {
		mailID := uuid.New()
		if _, err := tx.Exec(ctx, `
			INSERT INTO feedback_mails (id, event_id, ticket_id, manual, next_attempt_at, created_at) VALUES ($1, $2, $3, true, $4, $4)`,
			mailID, t.EventID, ticketID, now); err != nil {
			return false, 0, err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO feedback_mail_items (mail_id, event_id, session_id) VALUES ($1, $2, $3)`, mailID, t.EventID, t.SessionID); err != nil {
			return false, 0, err
		}
	}
	return true, len(tickets), tx.Commit(ctx)
}

// ClaimMail takes the mail of one of kinds due first and holds it for
// lease; false when none is due.
func (s *PostgresStore) ClaimMail(ctx context.Context, now time.Time, lease time.Duration, kinds []string) (Mail, bool, error) {
	m, err := scanMail(s.pool.QueryRow(ctx, `
		UPDATE feedback_mails SET next_attempt_at = $2, claimed_at = $1
		WHERE id = (
			SELECT id FROM feedback_mails
			WHERE done_at IS NULL AND next_attempt_at <= $1 AND kind = ANY($3)
			ORDER BY next_attempt_at, created_at, id
			LIMIT 1
			FOR UPDATE SKIP LOCKED)
		RETURNING `+mailCols,
		now, now.Add(lease), kinds))
	if errors.Is(err, pgx.ErrNoRows) {
		return Mail{}, false, nil
	}
	return m, err == nil, err
}

const mailCols = `id, event_id, ticket_id, kind, event_day_id, manual, created_at, next_attempt_at, attempts, claimed_at`

func scanMail(row pgx.Row) (Mail, error) {
	var m Mail
	var claimed *time.Time
	err := row.Scan(&m.ID, &m.EventID, &m.TicketID, &m.Kind, &m.EventDayID, &m.Manual, &m.CreatedAt, &m.NextAttemptAt, &m.Attempts, &claimed)
	if claimed != nil {
		m.ClaimedAt = *claimed
	}
	return m, err
}

// Siblings are the other open mails of the claimed one's Event, day, kind
// (request or reminder) and trigger (scheduled or manual), not held by
// another pass: the mails one
// address may share (decision of 2026-10-20: a person with a member and a
// guest Ticket gets one mail a day).
func (s *PostgresStore) Siblings(ctx context.Context, claimed Mail, now time.Time) ([]Mail, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+mailCols+` FROM feedback_mails
		WHERE done_at IS NULL AND id <> $1 AND event_id = $2 AND event_day_id IS NOT DISTINCT FROM $3 AND manual = $4
		  AND kind = $6 AND (claimed_at IS NULL OR next_attempt_at <= $5)
		ORDER BY created_at, id`, claimed.ID, claimed.EventID, claimed.EventDayID, claimed.Manual, now, claimed.Kind)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Mail, error) { return scanMail(row) })
}

// ClaimSibling takes a sibling for the pass that holds the mail it joins:
// false when another pass took it, or it was done with, meanwhile.
func (s *PostgresStore) ClaimSibling(ctx context.Context, id uuid.UUID, now time.Time, lease time.Duration) (Mail, bool, error) {
	m, err := scanMail(s.pool.QueryRow(ctx, `
		UPDATE feedback_mails SET claimed_at = $2, next_attempt_at = $3
		WHERE id = $1 AND done_at IS NULL AND (claimed_at IS NULL OR next_attempt_at <= $2)
		RETURNING `+mailCols, id, now, now.Add(lease)))
	if errors.Is(err, pgx.ErrNoRows) {
		return Mail{}, false, nil
	}
	return m, err == nil, err
}

// PostponeMail makes the claimed mail due at at, without counting a
// failure, and releases the claim: it waits for a sibling due later.
func (s *PostgresStore) PostponeMail(ctx context.Context, claimed Mail, at time.Time) error {
	_, err := s.pool.Exec(ctx, `UPDATE feedback_mails SET next_attempt_at = $3, claimed_at = NULL WHERE id = $1 AND claimed_at = $2`,
		claimed.ID, claimed.ClaimedAt, at)
	return err
}

// MailItems are the targets the mail carries, as they are now.
func (s *PostgresStore) MailItems(ctx context.Context, mailID uuid.UUID) ([]MailItem, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT i.event_id, i.session_id, t.form_id,
			e.name, e.status, e.archived_at IS NOT NULL, e.feedback_mode,
			COALESCE(s.title, ''),
			s.id IS NOT NULL AND (s.archived_at IS NOT NULL OR s.cancelled OR d.archived_at IS NOT NULL),
			COALESCE(s.feedback_form_url, e.feedback_form_url),
			COALESCE(s.feedback_auto_send, e.feedback_auto_send)
		FROM feedback_mail_items i
		JOIN feedback_mail_targets t ON t.event_id = i.event_id AND t.session_id IS NOT DISTINCT FROM i.session_id
		JOIN events e ON e.id = i.event_id
		LEFT JOIN sessions s ON s.id = i.session_id
		LEFT JOIN event_days d ON d.id = s.event_day_id
		WHERE i.mail_id = $1
		ORDER BY i.session_id IS NULL, s.start_time NULLS LAST, s.order_index, i.session_id`, mailID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (MailItem, error) {
		var it MailItem
		err := row.Scan(&it.EventID, &it.SessionID, &it.FormID, &it.EventName, &it.EventStatus, &it.EventArchived, &it.FeedbackMode,
			&it.SessionTitle, &it.SessionGone, &it.FormURL, &it.AutoSend)
		return it, err
	})
}

// Invite is the Ticket's invite to the target's form: the one written
// already (a retried mail sends the same link), or a new one.
func (s *PostgresStore) Invite(ctx context.Context, ticketID uuid.UUID, t Target, formID uuid.UUID) (Invite, error) {
	created, err := s.Create(ctx, Invite{TicketID: ticketID, EventID: t.EventID, SessionID: t.SessionID, FormID: formID})
	if !errors.Is(err, ErrExists) {
		return created, err
	}
	return scanInvite(s.pool.QueryRow(ctx, `
		SELECT `+inviteCols+` FROM feedback_invites
		WHERE ticket_id = $1 AND event_id = $2 AND session_id IS NOT DISTINCT FROM $3`, ticketID, t.EventID, t.SessionID))
}

// RenewMail holds the claimed mail until until, while its pass still holds
// it.
func (s *PostgresStore) RenewMail(ctx context.Context, claimed Mail, until time.Time) error {
	_, err := s.pool.Exec(ctx, `UPDATE feedback_mails SET next_attempt_at = $3 WHERE id = $1 AND claimed_at = $2`, claimed.ID, claimed.ClaimedAt, until)
	return err
}

// FinishMail marks the claimed mail done: sent (SkyMail took it; a request
// sent gets its reminder), or no longer to be sent.
func (s *PostgresStore) FinishMail(ctx context.Context, claimed Mail, at time.Time, sent bool) error {
	_, err := s.pool.Exec(ctx, `UPDATE feedback_mails SET done_at = $3, sent = $4, claimed_at = NULL WHERE id = $1 AND claimed_at = $2`,
		claimed.ID, claimed.ClaimedAt, at, sent)
	return err
}

// RetryMail counts a failure, makes the claimed mail due at at and
// releases the claim.
func (s *PostgresStore) RetryMail(ctx context.Context, claimed Mail, at time.Time) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE feedback_mails SET attempts = attempts + 1, next_attempt_at = $3, claimed_at = NULL
		WHERE id = $1 AND claimed_at = $2`, claimed.ID, claimed.ClaimedAt, at)
	return err
}

// formIDFromURL is the form of a forms address: its first path segment that
// is a UUID (as event.FormIDFromURL; this package does not import event).
func formIDFromURL(raw string) (uuid.UUID, bool) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return uuid.Nil, false
	}
	for _, part := range strings.Split(parsed.Path, "/") {
		if id, err := uuid.Parse(part); err == nil && id != uuid.Nil {
			return id, true
		}
	}
	return uuid.Nil, false
}

// DayDue is when the day's mail of the Tickets goes (S2): SendDelay plus the
// grace after the last of the day's current Sessions any of them checked in
// to ends; nil when they checked in to none that has an end. The worker
// asks it for the Tickets of one address, so that a person with a member
// and a guest Ticket gets their one mail after the last Session of either.
func (s *PostgresStore) DayDue(ctx context.Context, ticketIDs []uuid.UUID, day uuid.UUID) (*time.Time, error) {
	var latest *time.Time
	err := s.pool.QueryRow(ctx, `
		SELECT max(`+sessionEndsSQL+`)
		FROM ticket_checkins c
		JOIN sessions s ON s.id = c.session_id AND s.archived_at IS NULL AND NOT s.cancelled
		JOIN event_days d ON d.id = s.event_day_id
		JOIN events e ON e.id = d.event_id
		WHERE c.ticket_id = ANY($1) AND d.id = $2`, ticketIDs, day).Scan(&latest)
	if err != nil || latest == nil {
		return nil, err
	}
	due := latest.Add(SendDelay + dayGrace)
	return &due, nil
}
