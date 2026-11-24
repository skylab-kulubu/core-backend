package ticket

import (
	"context"
	"errors"
	"strings"

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
// (docs/event-capacity.md).
func (s *PostgresStore) Cancel(ctx context.Context, id uuid.UUID, by CancelledBy, reason string) (Ticket, bool, error) {
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
			if _, err := tx.Exec(ctx, `DELETE FROM ticket_notifications WHERE ticket_id = $1`, id); err != nil {
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
	got.CheckIns = []CheckIn{}
	return got, changed, nil
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
