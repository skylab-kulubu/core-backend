package certificate

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// EnqueueMail writes one row of certificate_mails. The table's two unique
// indexes (one issued mail per certificate, one queued mail per
// certificate) turn a repeat into nothing: false.
func (s *PostgresStore) EnqueueMail(ctx context.Context, m Mail) (bool, error) {
	tag, err := s.pool.Exec(ctx, `
		INSERT INTO certificate_mails (id, certificate_id, event_id, reason, requested_by)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT DO NOTHING`, m.ID, m.CertificateID, m.EventID, m.Reason, nullableUUIDPtr(m.RequestedBy))
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// ClaimMails takes due rows whose lease is free, counts the try and reads
// each one's certificate in the same statement.
func (s *PostgresStore) ClaimMails(ctx context.Context, limit int, lease time.Duration) ([]Mail, error) {
	if limit <= 0 {
		limit = 20
	}
	if lease <= 0 {
		lease = mailLease
	}
	token := uuid.New()
	rows, err := s.pool.Query(ctx, `
		WITH picked AS (
			SELECT id FROM certificate_mails
			WHERE status = 'queued' AND next_attempt_at <= now()
			  AND (claimed_until IS NULL OR claimed_until < now())
			ORDER BY next_attempt_at, created_at
			FOR UPDATE SKIP LOCKED
			LIMIT $1
		)
		UPDATE certificate_mails m
		SET attempts = m.attempts + 1, claimed_until = now() + make_interval(secs => $2),
			claim_token = $3, updated_at = now()
		FROM picked, certificates c
		WHERE m.id = picked.id AND c.id = m.certificate_id
		RETURNING m.id, m.certificate_id, m.event_id, m.reason, m.attempts, m.created_at,
			c.serial, c.recipient_name, c.recipient_email, c.event_name, c.owner_team, c.verify_url, c.revoked_at`,
		limit, lease.Seconds(), token)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Mail, 0)
	for rows.Next() {
		m := Mail{Status: MailQueued, ClaimToken: token}
		c := &m.Certificate
		if err := rows.Scan(&m.ID, &m.CertificateID, &m.EventID, &m.Reason, &m.Attempts, &m.CreatedAt,
			&c.Serial, &c.RecipientName, &c.RecipientEmail, &c.EventName, &c.OwnerTeam, &c.VerifyURL, &c.RevokedAt); err != nil {
			return nil, err
		}
		c.ID, c.EventID = m.CertificateID, m.EventID
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *PostgresStore) FinishMail(ctx context.Context, id, token uuid.UUID, status, errorCode string, retryAt time.Time) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE certificate_mails
		SET status = $3, error_code = $4, next_attempt_at = $5,
			claimed_until = NULL, claim_token = NULL, updated_at = now(),
			sent_at = CASE WHEN $3 = 'sent' THEN now() ELSE NULL END
		WHERE id = $1 AND claim_token = $2 AND status = 'queued'`, id, token, status, errorCode, retryAt)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrLeaseLost
	}
	return nil
}

func (s *PostgresStore) MailStates(ctx context.Context, eventID uuid.UUID) (map[string]MailState, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT DISTINCT ON (m.certificate_id) c.serial, m.status, m.attempts, m.error_code, m.sent_at, m.updated_at
		FROM certificate_mails m
		JOIN certificates c ON c.id = m.certificate_id
		WHERE m.event_id = $1
		ORDER BY m.certificate_id, m.created_at DESC, m.id DESC`, eventID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]MailState{}
	for rows.Next() {
		var serial string
		var state MailState
		if err := rows.Scan(&serial, &state.Status, &state.Attempts, &state.ErrorCode, &state.SentAt, &state.UpdatedAt); err != nil {
			return nil, err
		}
		out[serial] = state
	}
	return out, rows.Err()
}
