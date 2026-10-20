package commpref

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// TrackingChange is one row of the tracking audit trail: who turned tracking
// on or off, when and from where. No address, no IP.
type TrackingChange struct {
	Subject uuid.UUID
	Enabled bool
	// Previous is the value before, nil when there was no row (the mode's
	// default applied).
	Previous  *bool
	Source    Source
	ChangedAt time.Time
}

// Withdrawal is one tracking withdrawal waiting for SkyMail: the person
// turned tracking off at RequestedAt.
type Withdrawal struct {
	Subject       uuid.UUID
	RequestedAt   time.Time
	NextAttemptAt time.Time
	// Attempts counts the failed sends in a row.
	Attempts int
	// ClaimedAt is the claim that returned the withdrawal. Complete and
	// Retry write only while the row still carries it: a newer request for
	// the same person (which clears the claim) or another core's claim
	// makes a late write change nothing.
	ClaimedAt time.Time
}

// WithdrawalQueue keeps the withdrawals SkyMail has not confirmed yet (the
// outbox Apply writes in the transaction that turns tracking off).
type WithdrawalQueue interface {
	// ClaimWithdrawals takes up to limit due withdrawals, the oldest due
	// first, and holds them for lease.
	ClaimWithdrawals(ctx context.Context, now time.Time, lease time.Duration, limit int) ([]Withdrawal, error)
	// CompleteWithdrawal removes a claimed withdrawal SkyMail confirmed (or
	// refused for good).
	CompleteWithdrawal(ctx context.Context, claimed Withdrawal) error
	// RetryWithdrawal counts a failure, makes the withdrawal due at at and
	// releases the claim; status is the answer's HTTP status (0: none).
	RetryWithdrawal(ctx context.Context, claimed Withdrawal, at time.Time, status int) error
	// WithdrawalBacklog counts the waiting withdrawals and the age of the
	// oldest request.
	WithdrawalBacklog(ctx context.Context, now time.Time) (int, time.Duration, error)
}

// applyTracking writes a change of Tracking() inside Apply's transaction:
// the row, an audit row when the value changes, and a withdrawal when it
// turns tracking off. A withdrawal already waiting is replaced: due now,
// its failures forgotten, its claim cleared (SkyMail's call is idempotent).
func applyTracking(ctx context.Context, tx pgx.Tx, person uuid.UUID, change Change, source Source, now time.Time) error {
	var previous *bool
	err := tx.QueryRow(ctx, `
		SELECT enabled FROM communication_preferences
		WHERE user_id = $1 AND channel = $2 AND scope = 'tracking'
		FOR UPDATE`, person, string(change.Channel)).Scan(&previous)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if change.Enabled == nil {
		_, err := tx.Exec(ctx, `
			DELETE FROM communication_preferences WHERE user_id = $1 AND channel = $2 AND scope = 'tracking'`,
			person, string(change.Channel))
		return err
	}
	on := *change.Enabled
	if _, err := tx.Exec(ctx, `
		INSERT INTO communication_preferences (user_id, channel, scope, group_id, news_kind, enabled, source, updated_at)
		VALUES ($1, $2, 'tracking', NULL, NULL, $3, $4, $5)
		ON CONFLICT ON CONSTRAINT communication_preferences_key
		DO UPDATE SET enabled = EXCLUDED.enabled, source = EXCLUDED.source, updated_at = EXCLUDED.updated_at`,
		person, string(change.Channel), on, string(source), now); err != nil {
		return err
	}
	if previous == nil || *previous != on {
		if _, err := tx.Exec(ctx, `
			INSERT INTO communication_tracking_changes (user_id, enabled, previous, source, changed_at)
			VALUES ($1, $2, $3, $4, $5)`, person, on, previous, string(source), now); err != nil {
			return err
		}
	}
	if on {
		return nil
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO communication_tracking_withdrawals (user_id, requested_at, next_attempt_at)
		VALUES ($1, $2, $2)
		ON CONFLICT (user_id) DO UPDATE SET requested_at = EXCLUDED.requested_at,
			next_attempt_at = EXCLUDED.next_attempt_at, attempts = 0, claimed_at = NULL`, person, now)
	return err
}

func (s *PostgresStore) ClaimWithdrawals(ctx context.Context, now time.Time, lease time.Duration, limit int) ([]Withdrawal, error) {
	rows, err := s.pool.Query(ctx, `
		UPDATE communication_tracking_withdrawals SET next_attempt_at = $2, claimed_at = $1
		WHERE user_id IN (
			SELECT user_id FROM communication_tracking_withdrawals
			WHERE next_attempt_at <= $1
			ORDER BY next_attempt_at, requested_at, user_id
			LIMIT $3
			FOR UPDATE SKIP LOCKED)
		RETURNING user_id, requested_at, next_attempt_at, attempts, claimed_at`,
		now, now.Add(lease), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Withdrawal
	for rows.Next() {
		var w Withdrawal
		if err := rows.Scan(&w.Subject, &w.RequestedAt, &w.NextAttemptAt, &w.Attempts, &w.ClaimedAt); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

func (s *PostgresStore) CompleteWithdrawal(ctx context.Context, claimed Withdrawal) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM communication_tracking_withdrawals WHERE user_id = $1 AND claimed_at = $2`,
		claimed.Subject, claimed.ClaimedAt)
	return err
}

func (s *PostgresStore) RetryWithdrawal(ctx context.Context, claimed Withdrawal, at time.Time, status int) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE communication_tracking_withdrawals
		SET attempts = attempts + 1, next_attempt_at = $3, claimed_at = NULL, last_status = $4
		WHERE user_id = $1 AND claimed_at = $2`, claimed.Subject, claimed.ClaimedAt, at, status)
	return err
}

func (s *PostgresStore) WithdrawalBacklog(ctx context.Context, now time.Time) (int, time.Duration, error) {
	var count int
	var oldest *time.Time
	if err := s.pool.QueryRow(ctx, `SELECT count(*), min(requested_at) FROM communication_tracking_withdrawals`).Scan(&count, &oldest); err != nil {
		return 0, 0, err
	}
	if oldest == nil {
		return 0, 0, nil
	}
	return count, max(now.Sub(*oldest), 0), nil
}

// TrackingChanges is the audit trail of person, oldest first.
func (s *PostgresStore) TrackingChanges(ctx context.Context, person uuid.UUID) ([]TrackingChange, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT user_id, enabled, previous, source, changed_at FROM communication_tracking_changes
		WHERE user_id = $1 ORDER BY id`, person)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TrackingChange
	for rows.Next() {
		var c TrackingChange
		var source string
		if err := rows.Scan(&c.Subject, &c.Enabled, &c.Previous, &source, &c.ChangedAt); err != nil {
			return nil, err
		}
		c.Source = Source(source)
		out = append(out, c)
	}
	return out, rows.Err()
}

func (m *MemoryStore) ClaimWithdrawals(_ context.Context, now time.Time, lease time.Duration, limit int) ([]Withdrawal, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var due []Withdrawal
	for _, w := range m.withdrawals {
		if !w.NextAttemptAt.After(now) {
			due = append(due, w)
		}
	}
	slices.SortFunc(due, func(a, b Withdrawal) int {
		if c := a.NextAttemptAt.Compare(b.NextAttemptAt); c != 0 {
			return c
		}
		return strings.Compare(a.Subject.String(), b.Subject.String())
	})
	out := due[:min(limit, len(due))]
	for i := range out {
		out[i].NextAttemptAt, out[i].ClaimedAt = now.Add(lease), now
		m.withdrawals[out[i].Subject] = out[i]
	}
	return slices.Clone(out), nil
}

func (m *MemoryStore) CompleteWithdrawal(_ context.Context, claimed Withdrawal) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if w, ok := m.withdrawals[claimed.Subject]; ok && w.ClaimedAt.Equal(claimed.ClaimedAt) && !w.ClaimedAt.IsZero() {
		delete(m.withdrawals, claimed.Subject)
	}
	return nil
}

func (m *MemoryStore) RetryWithdrawal(_ context.Context, claimed Withdrawal, at time.Time, _ int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if w, ok := m.withdrawals[claimed.Subject]; ok && w.ClaimedAt.Equal(claimed.ClaimedAt) && !w.ClaimedAt.IsZero() {
		w.Attempts++
		w.NextAttemptAt, w.ClaimedAt = at, time.Time{}
		m.withdrawals[claimed.Subject] = w
	}
	return nil
}

func (m *MemoryStore) WithdrawalBacklog(_ context.Context, now time.Time) (int, time.Duration, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var oldest time.Time
	for _, w := range m.withdrawals {
		if oldest.IsZero() || w.RequestedAt.Before(oldest) {
			oldest = w.RequestedAt
		}
	}
	if len(m.withdrawals) == 0 {
		return 0, 0, nil
	}
	return len(m.withdrawals), max(now.Sub(oldest), 0), nil
}

// Withdrawals are the waiting withdrawals (tests).
func (m *MemoryStore) Withdrawals() []Withdrawal {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Withdrawal, 0, len(m.withdrawals))
	for _, w := range m.withdrawals {
		out = append(out, w)
	}
	return out
}

// TrackingChanges is the audit trail of person, oldest first.
func (m *MemoryStore) TrackingChanges(_ context.Context, person uuid.UUID) ([]TrackingChange, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []TrackingChange
	for _, c := range m.audit {
		if c.Subject == person {
			out = append(out, c)
		}
	}
	return out, nil
}

var (
	_ WithdrawalQueue = (*PostgresStore)(nil)
	_ WithdrawalQueue = (*MemoryStore)(nil)
)
