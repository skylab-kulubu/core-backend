package certificate

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// memoryMail is a MemoryStore row of the certificate mail queue.
type memoryMail struct {
	Mail
	nextAttempt  time.Time
	claimedUntil *time.Time
	createdAt    time.Time
	updatedAt    time.Time
	sentAt       *time.Time
}

func (s *MemoryStore) EnqueueMail(_ context.Context, m Mail) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.byID[m.CertificateID]; !ok {
		return false, ErrNotFound
	}
	for _, row := range s.mails {
		if row.CertificateID != m.CertificateID {
			continue
		}
		if row.Status == MailQueued || (m.Reason == MailReasonIssued && row.Reason == MailReasonIssued) {
			return false, nil
		}
	}
	now := time.Now().UTC()
	m.Status, m.Attempts, m.ErrorCode, m.ClaimToken, m.Certificate, m.FirstTriedAt = MailQueued, 0, "", uuid.Nil, Certificate{}, time.Time{}
	s.mails = append(s.mails, &memoryMail{Mail: m, nextAttempt: now, createdAt: now, updatedAt: now})
	return true, nil
}

func (s *MemoryStore) ClaimMails(_ context.Context, limit int, lease time.Duration) ([]Mail, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC()
	out := make([]Mail, 0)
	for _, row := range s.mails {
		if len(out) >= limit {
			break
		}
		if row.Status != MailQueued || row.nextAttempt.After(now) || (row.claimedUntil != nil && row.claimedUntil.After(now)) {
			continue
		}
		until := now.Add(lease)
		row.claimedUntil, row.ClaimToken, row.updatedAt = &until, uuid.New(), now
		row.Attempts++
		if row.FirstTriedAt.IsZero() {
			row.FirstTriedAt = now
		}
		claimed := row.Mail
		claimed.Certificate = s.byID[row.CertificateID].cert
		out = append(out, claimed)
	}
	return out, nil
}

func (s *MemoryStore) RenewMailLease(_ context.Context, id, token uuid.UUID, lease time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC()
	for _, row := range s.mails {
		if row.ID != id {
			continue
		}
		if row.Status != MailQueued || row.ClaimToken != token || row.claimedUntil == nil || !row.claimedUntil.After(now) {
			return ErrLeaseLost
		}
		until := now.Add(lease)
		row.claimedUntil = &until
		return nil
	}
	return ErrLeaseLost
}

func (s *MemoryStore) ResendEventMails(_ context.Context, eventID uuid.UUID, scope string, by *uuid.UUID, cooldown time.Duration) (MailResend, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC()
	if scope == ResendAll {
		if s.resendAllAt == nil {
			s.resendAllAt = map[uuid.UUID]time.Time{}
		}
		if last, ok := s.resendAllAt[eventID]; ok && now.Sub(last) < cooldown {
			return MailResend{}, ErrResendTooSoon
		}
		s.resendAllAt[eventID] = now
	}
	latest := map[uuid.UUID]*memoryMail{}
	pending := map[uuid.UUID]bool{}
	for _, row := range s.mails {
		latest[row.CertificateID] = row
		if row.Status == MailQueued {
			pending[row.CertificateID] = true
		}
	}
	var out MailResend
	for id, rec := range s.byID {
		if rec.cert.EventID != eventID || rec.cert.RevokedAt != nil {
			continue
		}
		if scope != ResendAll && (latest[id] == nil || latest[id].Status != MailFailed) {
			continue
		}
		if scope == ResendAll && rec.cert.RecipientEmail == "" {
			continue
		}
		if pending[id] {
			out.AlreadyQueued++
			continue
		}
		m := Mail{ID: uuid.New(), CertificateID: id, EventID: eventID, Reason: MailReasonResend, Status: MailQueued, RequestedBy: by}
		s.mails = append(s.mails, &memoryMail{Mail: m, nextAttempt: now, createdAt: now, updatedAt: now})
		out.Queued++
	}
	return out, nil
}

func (s *MemoryStore) FinishMail(_ context.Context, id, token uuid.UUID, status, errorCode string, retryAt time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, row := range s.mails {
		if row.ID != id {
			continue
		}
		if row.claimedUntil == nil || row.ClaimToken != token {
			return ErrLeaseLost
		}
		now := time.Now().UTC()
		row.Status, row.ErrorCode, row.nextAttempt, row.claimedUntil, row.ClaimToken, row.updatedAt = status, errorCode, retryAt, nil, uuid.Nil, now
		if status == MailSent {
			row.sentAt = &now
		}
		return nil
	}
	return ErrLeaseLost
}

func (s *MemoryStore) MailStates(_ context.Context, eventID uuid.UUID) (map[string]MailState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]MailState{}
	// Rows are kept in the order they were written: the last one wins.
	for _, row := range s.mails {
		if row.EventID != eventID {
			continue
		}
		rec, ok := s.byID[row.CertificateID]
		if !ok {
			continue
		}
		out[rec.cert.Serial] = MailState{Status: row.Status, Attempts: row.Attempts, ErrorCode: row.ErrorCode, SentAt: row.sentAt, UpdatedAt: row.updatedAt}
	}
	return out, nil
}
