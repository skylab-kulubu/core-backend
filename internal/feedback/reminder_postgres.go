package feedback

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// The feedback reminder (ticket 07, docs/event-feedback.md): three days
// after a feedback mail went, one reminder with the same links, to whom has
// not answered; never once the form closed (decision S3).

const (
	// ReminderDelay is how long after a feedback mail went its reminder
	// goes (K4).
	ReminderDelay = 3 * 24 * time.Hour
	// ReminderWindow bounds the reminders: one that fell due longer ago than
	// this (core was off meanwhile) is not sent.
	ReminderWindow = 7 * 24 * time.Hour
	// reminderLock is the advisory lock the reminders' planning takes: one
	// replica plans at a time.
	reminderLock = 7206119531226841861
	// maxRemindersPlanned is how many sent mails one pass plans the
	// reminders of; the next pass, a minute later, plans the rest.
	maxRemindersPlanned = 500
)

// PlanReminders first cancels for good the reminders of the forms whose
// closing time came (a closed form's invites are never reminded, even if it
// opens again). Then it queues the reminders that are due at now: for each
// feedback mail that went ReminderDelay ago (within ReminderWindow), one
// reminder of the same Ticket, day and trigger, with those of its targets
// whose invite is neither answered, reminded nor cancelled. A mail none of
// whose invites is left gets no reminder. Each mail is planned once. One
// replica plans at a time; another one skips. Answers how many reminders it
// queued.
func (s *PostgresStore) PlanReminders(ctx context.Context, now time.Time) (int, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	var locked bool
	if err := tx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock($1)`, int64(reminderLock)).Scan(&locked); err != nil || !locked {
		return 0, err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE feedback_invites fi SET reminder_cancelled_at = $1
		FROM feedback_form_closings c
		WHERE c.form_id = fi.form_id AND c.closes_at <= $1 AND `+reminderPendingSQL, now); err != nil {
		return 0, err
	}
	rows, err := tx.Query(ctx, `
		SELECT `+mailCols+` FROM feedback_mails
		WHERE kind = 'request' AND sent AND reminder_planned_at IS NULL
		  AND done_at <= $1 - $2::float8 * interval '1 second'
		  AND done_at > $1 - $3::float8 * interval '1 second'
		ORDER BY done_at, id
		LIMIT $4
		FOR UPDATE`, now, ReminderDelay.Seconds(), (ReminderDelay + ReminderWindow).Seconds(), maxRemindersPlanned)
	if err != nil {
		return 0, err
	}
	sentMails, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Mail, error) { return scanMail(row) })
	if err != nil {
		return 0, err
	}
	queued := 0
	for _, m := range sentMails {
		if _, err := tx.Exec(ctx, `UPDATE feedback_mails SET reminder_planned_at = $2 WHERE id = $1`, m.ID, now); err != nil {
			return 0, err
		}
		reminderID := uuid.New()
		tag, err := tx.Exec(ctx, `
			WITH left_over AS (
				SELECT i.event_id, i.session_id FROM feedback_mail_items i
				JOIN feedback_invites fi ON fi.ticket_id = $2 AND fi.event_id = i.event_id AND fi.session_id IS NOT DISTINCT FROM i.session_id
				WHERE i.mail_id = $1 AND `+reminderPendingSQL+`
			), reminder AS (
				INSERT INTO feedback_mails (id, event_id, ticket_id, kind, event_day_id, manual, next_attempt_at, created_at)
				SELECT $3, $4, $2, 'reminder', $5, $6, $7, $7 WHERE EXISTS (SELECT 1 FROM left_over)
				RETURNING id
			)
			INSERT INTO feedback_mail_items (mail_id, event_id, session_id)
			SELECT reminder.id, left_over.event_id, left_over.session_id FROM reminder, left_over`,
			m.ID, m.TicketID, reminderID, m.EventID, m.EventDayID, m.Manual, now)
		if err != nil {
			return 0, err
		}
		if tag.RowsAffected() > 0 {
			queued++
		}
	}
	return queued, tx.Commit(ctx)
}

// reminderPendingSQL is an invite (alias fi) whose reminder may still go:
// not answered, not reminded, not cancelled.
const reminderPendingSQL = `fi.answered_at IS NULL AND fi.reminded_at IS NULL AND fi.reminder_cancelled_at IS NULL`

// ReminderInvite is the Ticket's invite to the target when its reminder may
// go at now: not answered, reminded or cancelled, its form not closed by
// now. False otherwise.
func (s *PostgresStore) ReminderInvite(ctx context.Context, ticketID uuid.UUID, t Target, now time.Time) (Invite, bool, error) {
	inv, err := scanInvite(s.pool.QueryRow(ctx, `
		SELECT `+inviteCols+` FROM feedback_invites fi
		WHERE ticket_id = $1 AND event_id = $2 AND session_id IS NOT DISTINCT FROM $3 AND `+reminderPendingSQL+`
		  AND NOT EXISTS (SELECT 1 FROM feedback_form_closings c WHERE c.form_id = fi.form_id AND c.closes_at <= $4)`,
		ticketID, t.EventID, t.SessionID, now))
	if errors.Is(err, pgx.ErrNoRows) {
		return Invite{}, false, nil
	}
	return inv, err == nil, err
}

// MarkReminded records the invites' reminder as sent.
func (s *PostgresStore) MarkReminded(ctx context.Context, ids []uuid.UUID, at time.Time) error {
	_, err := s.pool.Exec(ctx, `UPDATE feedback_invites SET reminded_at = COALESCE(reminded_at, $2) WHERE id = ANY($1)`, ids, at)
	return err
}

// CancelReminders cancels for good the reminders still to go of the
// target's invites: its form was closed in the panel. Answers how many.
func (s *PostgresStore) CancelReminders(ctx context.Context, t Target, at time.Time) (int, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE feedback_invites fi SET reminder_cancelled_at = $3
		WHERE event_id = $1 AND session_id IS NOT DISTINCT FROM $2 AND `+reminderPendingSQL, t.EventID, t.SessionID, at)
	return int(tag.RowsAffected()), err
}

// ReportFormState takes the forms service's word that a form opened or
// closed: closed (open false) now; open, closing at closesAt; or open with
// no closing time. Only a feedback form's state is kept: a current Event's
// or Session's feedback form, or a form invites went out for. From its
// closing time on, its invites' reminders are cancelled for good (at once
// when that time has come, else by the next PlanReminders after it). An
// open form with no closing time deletes its closing; a reminder cancelled
// already stays cancelled. Answers how many reminders it cancelled now.
func (s *PostgresStore) ReportFormState(ctx context.Context, formID uuid.UUID, open bool, closesAt *time.Time, now time.Time) (int, error) {
	if formID == uuid.Nil {
		return 0, ErrInvalid
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	var known bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM feedback_invites WHERE form_id = $1)
		    OR EXISTS (SELECT 1 FROM events WHERE archived_at IS NULL AND feedback_form_url <> '' AND strpos(lower(feedback_form_url), $2) > 0)
		    OR EXISTS (SELECT 1 FROM sessions WHERE archived_at IS NULL AND feedback_form_url <> '' AND strpos(lower(feedback_form_url), $2) > 0)`,
		formID, formID.String()).Scan(&known); err != nil {
		return 0, err
	}
	if !known {
		return 0, nil
	}
	switch {
	case !open:
		_, err = tx.Exec(ctx, `
			INSERT INTO feedback_form_closings AS c (form_id, closes_at, updated_at) VALUES ($1, $2, $2)
			ON CONFLICT (form_id) DO UPDATE SET closes_at = LEAST(c.closes_at, EXCLUDED.closes_at), updated_at = EXCLUDED.updated_at`, formID, now)
	case closesAt != nil:
		_, err = tx.Exec(ctx, `
			INSERT INTO feedback_form_closings (form_id, closes_at, updated_at) VALUES ($1, $2, $3)
			ON CONFLICT (form_id) DO UPDATE SET closes_at = EXCLUDED.closes_at, updated_at = EXCLUDED.updated_at`, formID, closesAt.UTC(), now)
	default:
		_, err = tx.Exec(ctx, `DELETE FROM feedback_form_closings WHERE form_id = $1`, formID)
	}
	if err != nil {
		return 0, err
	}
	tag, err := tx.Exec(ctx, `
		UPDATE feedback_invites fi SET reminder_cancelled_at = $2
		FROM feedback_form_closings c
		WHERE c.form_id = $1 AND fi.form_id = $1 AND c.closes_at <= $2 AND `+reminderPendingSQL, formID, now)
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), tx.Commit(ctx)
}
