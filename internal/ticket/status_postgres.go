package ticket

import (
	"context"
	"errors"
	"log"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/skylab-kulubu/core-backend/internal/event"
)

// Cancel cancels the Ticket (docs/ticket-cancellation.md) in one
// transaction that holds its Event and then it: a Ticket with a check-in is
// ErrTicketCheckedIn, one in a Competition's current Entrant
// ErrTicketInCompetition. A Ticket already cancelled is answered as it is,
// changed false. Its mail still waiting is dropped with it. An active
// Ticket's place goes to the Event's waiting list in the same transaction
// (docs/event-capacity.md). It tells nobody (CancelTelling).
func (s *PostgresStore) Cancel(ctx context.Context, id uuid.UUID, by CancelledBy, reason string) (Ticket, bool, error) {
	return s.CancelTelling(ctx, id, by, reason, false)
}

// CancelTelling is Cancel that, when tell, queues the holder's
// "Biletin iptal edildi" mail in its place while the status mails are on
// (docs/ticket-status-mail.md); off, the log says the mail did not go,
// naming the Event alone.
func (s *PostgresStore) CancelTelling(ctx context.Context, id uuid.UUID, by CancelledBy, reason string, tell bool) (Ticket, bool, error) {
	var (
		got     Ticket
		changed bool
	)
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := lockEvent(ctx, tx, id); err != nil {
			return err
		}
		status, err := lockStatus(ctx, tx, id)
		if err != nil {
			return err
		}
		if status != StatusCancelled {
			var checkedIn, entrant bool
			if err := tx.QueryRow(ctx, `
				SELECT EXISTS (SELECT 1 FROM ticket_checkins WHERE ticket_id = $1),
				       EXISTS (SELECT 1 FROM competition_entrant_members m
				               JOIN competition_entrants e ON e.id = m.entrant_id
				               WHERE m.ticket_id = $1 AND e.status = 'active')`, id).Scan(&checkedIn, &entrant); err != nil {
				return err
			}
			switch {
			case checkedIn:
				return ErrTicketCheckedIn
			case entrant:
				return ErrTicketInCompetition
			}
			if _, err := tx.Exec(ctx, `
				UPDATE tickets SET status = 'cancelled', status_changed_at = now(), waitlisted_at = NULL,
					cancelled_at = now(), cancelled_by = $2, cancel_reason = NULLIF($3, ''), updated_at = now()
				WHERE id = $1`, id, string(by), strings.TrimSpace(reason)); err != nil {
				return err
			}
			if tell && s.notifyStatus.Load() {
				// The row of a mail still waiting ("Biletin hazır") becomes
				// the cancellation's: what was not sent is no longer news.
				err = event.QueueTicketMail(ctx, tx, id, "cancelled")
			} else {
				_, err = tx.Exec(ctx, `DELETE FROM ticket_notifications WHERE ticket_id = $1`, id)
			}
			if err != nil {
				return err
			}
			changed = true
		}
		got, err = scanTicket(tx.QueryRow(ctx, `SELECT `+ticketCols+` FROM tickets WHERE id = $1`, id))
		if err != nil {
			return err
		}
		if status == StatusActive {
			_, err = event.PromoteWaitlist(ctx, tx, got.EventID, s.promotionMail())
		}
		return err
	})
	if err != nil {
		return Ticket{}, false, err
	}
	if changed && tell && !s.notifyStatus.Load() {
		log.Printf("event %s: a ticket was cancelled; no mail (TICKET_STATUS_MAIL is not on)", got.EventID)
	}
	got.CheckIns = []CheckIn{}
	return got, changed, nil
}

// CancelBlocked is CancelBlockStore's: the Tickets of ids with a check-in
// or of a Competition's current Entrant, in one query (Cancel's test).
func (s *PostgresStore) CancelBlocked(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]bool, error) {
	out := map[uuid.UUID]bool{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.pool.Query(ctx, `
		SELECT ticket_id FROM ticket_checkins WHERE ticket_id = ANY($1)
		UNION
		SELECT m.ticket_id FROM competition_entrant_members m
		JOIN competition_entrants e ON e.id = m.entrant_id
		WHERE m.ticket_id = ANY($1) AND e.status = 'active'`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// FormResponseTickets are the Tickets the forms answer responseID wrote or
// reopened, of any status.
func (s *PostgresStore) FormResponseTickets(ctx context.Context, responseID uuid.UUID) ([]Ticket, error) {
	if responseID == uuid.Nil {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx, `SELECT `+ticketCols+` FROM tickets WHERE form_response_id = $1`, responseID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Ticket
	for rows.Next() {
		t, err := scanTicket(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// MarkFormsRejected marks a Ticket that is not cancelled declined in
// Forms (FormsRejectionStore); false when it is cancelled or marked.
func (s *PostgresStore) MarkFormsRejected(ctx context.Context, id uuid.UUID, at time.Time) (bool, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE tickets SET forms_rejected_at = $2, updated_at = now()
		WHERE id = $1 AND status <> 'cancelled' AND forms_rejected_at IS NULL`, id, at.UTC())
	return tag.RowsAffected() > 0, err
}

// SetFormsPending marks or clears the pending mark of an answer's Tickets
// (FormsPendingStore).
func (s *PostgresStore) SetFormsPending(ctx context.Context, responseID uuid.UUID, at *time.Time) (int, error) {
	if responseID == uuid.Nil {
		return 0, nil
	}
	var tagRows int64
	if at == nil {
		tag, err := s.pool.Exec(ctx, `
			UPDATE tickets SET forms_pending_at = NULL, updated_at = now()
			WHERE form_response_id = $1 AND forms_pending_at IS NOT NULL`, responseID)
		if err != nil {
			return 0, err
		}
		tagRows = tag.RowsAffected()
	} else {
		tag, err := s.pool.Exec(ctx, `
			UPDATE tickets SET forms_pending_at = $2, updated_at = now()
			WHERE form_response_id = $1 AND status <> 'cancelled' AND forms_pending_at IS NULL`, responseID, at.UTC())
		if err != nil {
			return 0, err
		}
		tagRows = tag.RowsAffected()
	}
	return int(tagRows), nil
}

// KeepFormResponse binds a Ticket an answer wrote to the holder's newest
// accepted answer and clears its mark (FormsRejectionStore).
func (s *PostgresStore) KeepFormResponse(ctx context.Context, id, responseID uuid.UUID) error {
	if responseID == uuid.Nil {
		return nil
	}
	_, err := s.pool.Exec(ctx, `
		UPDATE tickets SET form_response_id = $2, forms_rejected_at = NULL, updated_at = now()
		WHERE id = $1 AND form_response_id IS NOT NULL AND status <> 'cancelled'
		  AND (form_response_id <> $2 OR forms_rejected_at IS NOT NULL)`, id, responseID)
	return err
}

// Reopen makes a cancelled Ticket active again, for an operator taking the
// cancellation back: past the capacity if it must (Ticket.OverCapacity), a
// new access key and the next QR version (the old link answers 404, the old
// QR 410), and, while the Notifications are on, its "Biletin hazır" mail
// queued under the next resend number, so the mail goes again with the new
// link. A Ticket that is not cancelled is answered as it is, changed false.
func (s *PostgresStore) Reopen(ctx context.Context, id uuid.UUID) (Ticket, bool, error) {
	return s.ReopenAdmitted(ctx, id, Admission{Over: true})
}

// queueReopened queues a reopened Ticket's mail under the next resend
// number (its key …:r<n>, docs/ticket-resend.md): the first key was used
// by the mail before the cancellation. It is not a resend of the panel's,
// so the resend limits do not apply.
func queueReopened(ctx context.Context, tx pgx.Tx, id uuid.UUID) error {
	var n int
	if err := tx.QueryRow(ctx, `
		INSERT INTO ticket_resends (ticket_id, n, renewed)
		SELECT $1, COALESCE(max(n), 0) + 1, true FROM ticket_resends WHERE ticket_id = $1
		RETURNING n`, id).Scan(&n); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO ticket_notifications (ticket_id, resend) VALUES ($1, $2)
		ON CONFLICT (ticket_id) DO UPDATE
		SET resend = EXCLUDED.resend, created_at = now(), next_attempt_at = now(), attempts = 0, claimed_at = NULL`, id, n)
	return err
}

// statusOf is the Ticket's status, for the door's refusal: ErrNotFound for
// no such Ticket.
func (s *PostgresStore) statusOf(ctx context.Context, id uuid.UUID) (Status, error) {
	var status Status
	err := s.pool.QueryRow(ctx, `SELECT status FROM tickets WHERE id = $1`, id).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	return status, err
}
