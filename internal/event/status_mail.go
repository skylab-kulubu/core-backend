package event

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// StatusMailKind is what a status mail tells an Event's Ticket holders
// (ADR 0066 addendum 2026-10-19, decision 1; ticket 14).
type StatusMailKind string

const (
	// StatusMailCancelled: the Event is cancelled, with its reason.
	StatusMailCancelled StatusMailKind = "cancelled"
	// StatusMailReinstated: the cancellation is undone; it goes only to
	// whom the cancellation mail it follows reached.
	StatusMailReinstated StatusMailKind = "reinstated"
)

// StatusMail is one queued status mail of an Event. It holds ids only: the
// recipients and their addresses are read when it is sent.
type StatusMail struct {
	ID      uuid.UUID
	EventID uuid.UUID
	Kind    StatusMailKind
	// Follows is the cancellation mail a reinstated mail answers.
	Follows       *uuid.UUID
	CreatedAt     time.Time
	NextAttemptAt time.Time
	Attempts      int
	// ClaimedAt is the claim that returned the mail: Finish and Retry write
	// only while the row still carries it.
	ClaimedAt time.Time
}

// StatusMailQueue keeps the status mails: Cancel and Uncancel queue them in
// their own write, a worker (internal/eventstatusmail) sends them.
type StatusMailQueue interface {
	// ClaimStatusMail takes the mail due first and holds it for lease;
	// false when none is due. A reinstated mail waits until the
	// cancellation mail it follows is done, so it never overtakes it.
	ClaimStatusMail(ctx context.Context, now time.Time, lease time.Duration) (StatusMail, bool, error)
	// RenewStatusMail holds the claimed mail until until, while its pass
	// still holds it.
	RenewStatusMail(ctx context.Context, claimed StatusMail, until time.Time) error
	// NewerCancellationMail reports whether the Event has a cancellation
	// mail queued after after: a later cancellation's.
	NewerCancellationMail(ctx context.Context, eventID uuid.UUID, after time.Time) (bool, error)
	// StatusMailSends are the Tickets mail reached.
	StatusMailSends(ctx context.Context, mailID uuid.UUID) ([]uuid.UUID, error)
	// RecordStatusMailSend records that mail reached the Ticket.
	RecordStatusMailSend(ctx context.Context, mailID, ticketID uuid.UUID) error
	// FinishStatusMail marks the claimed mail done; it is kept for the
	// record of whom it reached.
	FinishStatusMail(ctx context.Context, claimed StatusMail, at time.Time) error
	// RetryStatusMail counts a failure, makes the claimed mail due at at
	// and releases the claim.
	RetryStatusMail(ctx context.Context, claimed StatusMail, at time.Time) error
}

func (s *PostgresStore) ClaimStatusMail(ctx context.Context, now time.Time, lease time.Duration) (StatusMail, bool, error) {
	var m StatusMail
	var kind string
	err := s.pool.QueryRow(ctx, `
		UPDATE event_status_mails SET next_attempt_at = $2, claimed_at = $1
		WHERE id = (
			SELECT id FROM event_status_mails m
			WHERE done_at IS NULL AND next_attempt_at <= $1
			  AND (follows_mail_id IS NULL OR EXISTS (
				SELECT 1 FROM event_status_mails f WHERE f.id = m.follows_mail_id AND f.done_at IS NOT NULL))
			ORDER BY next_attempt_at, created_at, id
			LIMIT 1
			FOR UPDATE SKIP LOCKED)
		RETURNING id, event_id, kind, follows_mail_id, created_at, next_attempt_at, attempts, claimed_at`,
		now, now.Add(lease)).Scan(&m.ID, &m.EventID, &kind, &m.Follows, &m.CreatedAt, &m.NextAttemptAt, &m.Attempts, &m.ClaimedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return StatusMail{}, false, nil
	}
	if err != nil {
		return StatusMail{}, false, err
	}
	m.Kind = StatusMailKind(kind)
	return m, true, nil
}

func (s *PostgresStore) RenewStatusMail(ctx context.Context, claimed StatusMail, until time.Time) error {
	_, err := s.pool.Exec(ctx, `UPDATE event_status_mails SET next_attempt_at = $3 WHERE id = $1 AND claimed_at = $2`,
		claimed.ID, claimed.ClaimedAt, until)
	return err
}

func (s *PostgresStore) NewerCancellationMail(ctx context.Context, eventID uuid.UUID, after time.Time) (bool, error) {
	var newer bool
	err := s.pool.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM event_status_mails WHERE event_id = $1 AND kind = 'cancelled' AND created_at > $2)`,
		eventID, after).Scan(&newer)
	return newer, err
}

func (s *PostgresStore) StatusMailSends(ctx context.Context, mailID uuid.UUID) ([]uuid.UUID, error) {
	rows, err := s.pool.Query(ctx, `SELECT ticket_id FROM event_status_mail_sends WHERE mail_id = $1 ORDER BY sent_at, ticket_id`, mailID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []uuid.UUID{}
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func (s *PostgresStore) RecordStatusMailSend(ctx context.Context, mailID, ticketID uuid.UUID) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO event_status_mail_sends (mail_id, ticket_id) VALUES ($1, $2) ON CONFLICT DO NOTHING`, mailID, ticketID)
	return err
}

func (s *PostgresStore) FinishStatusMail(ctx context.Context, claimed StatusMail, at time.Time) error {
	_, err := s.pool.Exec(ctx, `UPDATE event_status_mails SET done_at = $3, claimed_at = NULL WHERE id = $1 AND claimed_at = $2`,
		claimed.ID, claimed.ClaimedAt, at)
	return err
}

func (s *PostgresStore) RetryStatusMail(ctx context.Context, claimed StatusMail, at time.Time) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE event_status_mails SET attempts = attempts + 1, next_attempt_at = $3, claimed_at = NULL
		WHERE id = $1 AND claimed_at = $2`, claimed.ID, claimed.ClaimedAt, at)
	return err
}

// memoryStatusMail is a status mail in a MemoryStore.
type memoryStatusMail struct {
	StatusMail
	done  bool
	sends []uuid.UUID
}

// queueStatusMail queues kind's mail of the Event; the caller holds s.mu.
func (s *MemoryStore) queueStatusMail(eventID uuid.UUID, kind StatusMailKind, follows *uuid.UUID, now time.Time) uuid.UUID {
	id := uuid.New()
	s.statusMails = append(s.statusMails, &memoryStatusMail{StatusMail: StatusMail{
		ID: id, EventID: eventID, Kind: kind, Follows: follows, CreatedAt: now, NextAttemptAt: now,
	}})
	return id
}

// StatusMails are the Event's status mails, oldest first (for tests).
func (s *MemoryStore) StatusMails(eventID uuid.UUID) []StatusMail {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []StatusMail
	for _, m := range s.statusMails {
		if m.EventID == eventID {
			out = append(out, m.StatusMail)
		}
	}
	return out
}

func (s *MemoryStore) ClaimStatusMail(_ context.Context, now time.Time, lease time.Duration) (StatusMail, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	done := func(id *uuid.UUID) bool {
		for _, m := range s.statusMails {
			if id != nil && m.ID == *id {
				return m.done
			}
		}
		return true
	}
	var due *memoryStatusMail
	for _, m := range s.statusMails {
		if !m.done && !m.NextAttemptAt.After(now) && done(m.Follows) && (due == nil || m.NextAttemptAt.Before(due.NextAttemptAt)) {
			due = m
		}
	}
	if due == nil {
		return StatusMail{}, false, nil
	}
	due.NextAttemptAt, due.ClaimedAt = now.Add(lease), now
	return due.StatusMail, true, nil
}

func (s *MemoryStore) RenewStatusMail(_ context.Context, claimed StatusMail, until time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, m := range s.statusMails {
		if m.ID == claimed.ID && m.ClaimedAt.Equal(claimed.ClaimedAt) {
			m.NextAttemptAt = until
		}
	}
	return nil
}

func (s *MemoryStore) NewerCancellationMail(_ context.Context, eventID uuid.UUID, after time.Time) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, m := range s.statusMails {
		if m.EventID == eventID && m.Kind == StatusMailCancelled && m.CreatedAt.After(after) {
			return true, nil
		}
	}
	return false, nil
}

func (s *MemoryStore) StatusMailSends(_ context.Context, mailID uuid.UUID) ([]uuid.UUID, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, m := range s.statusMails {
		if m.ID == mailID {
			return slices.Clone(m.sends), nil
		}
	}
	return []uuid.UUID{}, nil
}

func (s *MemoryStore) RecordStatusMailSend(_ context.Context, mailID, ticketID uuid.UUID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, m := range s.statusMails {
		if m.ID == mailID && !slices.Contains(m.sends, ticketID) {
			m.sends = append(m.sends, ticketID)
		}
	}
	return nil
}

func (s *MemoryStore) FinishStatusMail(_ context.Context, claimed StatusMail, _ time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, m := range s.statusMails {
		if m.ID == claimed.ID && m.ClaimedAt.Equal(claimed.ClaimedAt) {
			m.done, m.ClaimedAt = true, time.Time{}
		}
	}
	return nil
}

func (s *MemoryStore) RetryStatusMail(_ context.Context, claimed StatusMail, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, m := range s.statusMails {
		if m.ID == claimed.ID && m.ClaimedAt.Equal(claimed.ClaimedAt) {
			m.Attempts++
			m.NextAttemptAt, m.ClaimedAt = at, time.Time{}
		}
	}
	return nil
}

var (
	_ StatusMailQueue = (*PostgresStore)(nil)
	_ StatusMailQueue = (*MemoryStore)(nil)
)
