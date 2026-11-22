package ticket

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// NotificationsOn reports whether new Tickets' Notifications are sent
// (TICKET_CREATED_NOTIFICATION): sending a Ticket's mail again needs them.
func (s *PostgresStore) NotificationsOn() bool { return s.notifyCreated.Load() }

// QueueResend records the next resend of the Ticket and queues its
// Notification (docs/ticket-resend.md), in one transaction that holds the
// Ticket. A request whose hash was seen for the Ticket is answered as it
// was, with no second row. limit resends in window: ErrResendLimited.
func (s *PostgresStore) QueueResend(ctx context.Context, id uuid.UUID, requestHash string, limit int, window time.Duration) (Resend, error) {
	var out Resend
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := lockTicket(ctx, tx, id); err != nil {
			return err
		}
		var err error
		out, err = queueResend(ctx, tx, id, requestHash, false, limit, window)
		return err
	})
	return out, err
}

// RenewAccessKey gives the Ticket a new access key and the next QR version
// (its code stays): the old page answers 404 and the old Ticket QR 410. With
// notify it also queues the Ticket's mail again, as QueueResend, in the
// same transaction; a request whose hash was seen is answered as it was,
// without renewing again.
func (s *PostgresStore) RenewAccessKey(ctx context.Context, id uuid.UUID, notify bool, requestHash string, limit int, window time.Duration) (Ticket, Resend, error) {
	var (
		got Ticket
		out Resend
	)
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := lockTicket(ctx, tx, id); err != nil {
			return err
		}
		if notify {
			var err error
			out, err = queueResend(ctx, tx, id, requestHash, true, limit, window)
			if err != nil {
				return err
			}
		}
		if !out.Replayed {
			key, err := s.newAccessKey()
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE tickets SET access_key = $2, qr_version = qr_version + 1, updated_at = now() WHERE id = $1`, id, key); err != nil {
				return err
			}
		}
		var err error
		got, err = scanTicket(tx.QueryRow(ctx, `SELECT `+ticketCols+` FROM tickets WHERE id = $1`, id))
		return err
	})
	if err != nil {
		return Ticket{}, Resend{}, err
	}
	got.CheckIns = []CheckIn{}
	return got, out, nil
}

func lockTicket(ctx context.Context, tx pgx.Tx, id uuid.UUID) error {
	var locked uuid.UUID
	err := tx.QueryRow(ctx, `SELECT id FROM tickets WHERE id = $1 FOR NO KEY UPDATE`, id).Scan(&locked)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

func queueResend(ctx context.Context, tx pgx.Tx, id uuid.UUID, requestHash string, renewed bool, limit int, window time.Duration) (Resend, error) {
	var hash *string
	if requestHash != "" {
		hash = &requestHash
		var n int
		err := tx.QueryRow(ctx, `SELECT n FROM ticket_resends WHERE ticket_id = $1 AND request_hash = $2`, id, requestHash).Scan(&n)
		if err == nil {
			return Resend{N: n, Replayed: true, Queued: true}, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return Resend{}, err
		}
	}
	var recent, last int
	if err := tx.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE created_at > now() - make_interval(secs => $2)), COALESCE(max(n), 0)
		FROM ticket_resends WHERE ticket_id = $1`, id, window.Seconds()).Scan(&recent, &last); err != nil {
		return Resend{}, err
	}
	if recent >= limit {
		return Resend{}, ErrResendLimited
	}
	n := last + 1
	if _, err := tx.Exec(ctx, `INSERT INTO ticket_resends (ticket_id, n, renewed, request_hash) VALUES ($1, $2, $3, $4)`, id, n, renewed, hash); err != nil {
		return Resend{}, err
	}
	// A row still waiting (the first mail, or an earlier resend) is taken
	// over: it now sends this resend, under its key.
	if _, err := tx.Exec(ctx, `
		INSERT INTO ticket_notifications (ticket_id, resend) VALUES ($1, $2)
		ON CONFLICT (ticket_id) DO UPDATE
		SET resend = EXCLUDED.resend, created_at = now(), next_attempt_at = now(), attempts = 0, claimed_at = NULL`, id, n); err != nil {
		return Resend{}, err
	}
	return Resend{N: n, Queued: true}, nil
}
