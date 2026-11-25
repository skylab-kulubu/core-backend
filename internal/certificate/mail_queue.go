package certificate

import (
	"context"
	"errors"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/mail"
	"github.com/skylab-kulubu/core-backend/internal/organizer"
)

// The certificate mail's states (docs/certificate-mail.md).
const (
	MailQueued    = "queued"
	MailSent      = "sent"
	MailFailed    = "failed"
	MailCancelled = "cancelled"
)

// Why a certificate mail was queued: its certificate was issued, or someone
// asked for it again from the panel.
const (
	MailReasonIssued = "issued"
	MailReasonResend = "resend"
)

// Which certificates "yeniden gönder" takes for an Event: those whose latest
// mail failed, or every active one.
const (
	ResendFailed = "failed"
	ResendAll    = "all"
)

// MailRetryWindow is how long a mail is tried while SkyNotify is unreachable,
// from its first try, before it is given up as failed: tries 1, 2, 4 … 60
// minutes apart, then hourly. It ends well before MailKeyLimit.
const MailRetryWindow = 24 * time.Hour

// MailKeyLimit is the hard end of a mail's tries, from its first: SkyNotify
// keeps an Idempotency-Key 72 hours, and a try after that could mail twice.
// A row first tried longer ago than this (a worker stopped for days) fails
// as "expired" without being sent.
const MailKeyLimit = 48 * time.Hour

// MailSendTimeout bounds one request core makes to send a mail (the token,
// a post, the template id's post): cmd/core-backend's skyNotifyTimeout.
const MailSendTimeout = 15 * time.Second

// mailSendBudget is the most one mail's send may take: the token and two
// posts. Each send runs under it.
const mailSendBudget = 3 * MailSendTimeout

// mailRowBudget is the most one mail may take: reading its PDF
// (CERTIFICATE_PDF_ATTACHMENT, at most MailPDFTimeout) and its send.
const mailRowBudget = MailPDFTimeout + mailSendBudget

// MailBatch is how many mails one claim takes (MaintainMails).
const MailBatch = 5

// mailLeaseFor is how long a claim of n mails holds them: every send at
// its longest, and a minute's slack. Each send renews its own mail's lease
// first and skips a mail whose lease ran out, so a slow batch never sends
// a mail another claim has taken.
func mailLeaseFor(n int) time.Duration {
	return time.Duration(max(n, 1))*mailRowBudget + time.Minute
}

// ResendAllCooldown is how often "Herkese yeniden gönder" may run for one
// Event: a second one within it is refused (ErrResendTooSoon).
const ResendAllCooldown = 10 * time.Minute

// ErrResendTooSoon: "Herkese yeniden gönder" ran for the Event less than
// ResendAllCooldown ago.
var ErrResendTooSoon = errors.New("certificate: resend to all too soon")

// MailIdempotencyKeyPrefix names a queued mail at SkyNotify: the prefix and
// the row's id, the same on every try.
const MailIdempotencyKeyPrefix = "core-certificate-mail:"

// Mail is one row of the certificate mail queue. Certificate is the
// certificate it is about, read when it is claimed: the row itself holds no
// address or name.
type Mail struct {
	ID            uuid.UUID
	CertificateID uuid.UUID
	EventID       uuid.UUID
	Reason        string
	Status        string
	Attempts      int
	ErrorCode     string
	RequestedBy   *uuid.UUID
	ClaimToken    uuid.UUID
	// FirstTriedAt is its first try's time (the claim's, on the first):
	// MailRetryWindow and MailKeyLimit run from it.
	FirstTriedAt time.Time
	Certificate  Certificate
}

// MailState is a certificate's latest mail as the panel shows it.
type MailState struct {
	Status    string     `json:"status"`
	Attempts  int        `json:"attempts"`
	ErrorCode string     `json:"errorCode,omitempty"`
	SentAt    *time.Time `json:"sentAt,omitempty"`
	UpdatedAt time.Time  `json:"updatedAt"`
}

// MailResend is what "yeniden gönder" did: Queued mails written, and
// AlreadyQueued certificates that had one waiting (it is not doubled).
type MailResend struct {
	Queued        int `json:"queued"`
	AlreadyQueued int `json:"alreadyQueued"`
}

// MailQueue keeps the certificate mails (PostgresStore, MemoryStore).
type MailQueue interface {
	// EnqueueMail writes m unless its certificate has a queued mail, or,
	// for MailReasonIssued, any issued mail already: false then.
	EnqueueMail(ctx context.Context, m Mail) (bool, error)
	// ClaimMails takes up to limit due mails for lease, each with its
	// certificate, counting the try.
	ClaimMails(ctx context.Context, limit int, lease time.Duration) ([]Mail, error)
	// RenewMailLease holds a claimed mail for lease from now. ErrLeaseLost
	// unless it is claimed under token and its lease has not run out.
	RenewMailLease(ctx context.Context, id, token uuid.UUID, lease time.Duration) error
	// ResendEventMails queues the Event's active certificates' mails again
	// (scope ResendFailed or ResendAll) in one statement, skipping those
	// with a mail waiting. ResendAll is refused with ErrResendTooSoon within
	// cooldown of the Event's last one.
	ResendEventMails(ctx context.Context, eventID uuid.UUID, scope string, by *uuid.UUID, cooldown time.Duration) (MailResend, error)
	// FinishMail settles a claimed mail: MailSent, MailFailed or
	// MailCancelled, or MailQueued again at retryAt. ErrLeaseLost unless it
	// is still claimed under token.
	FinishMail(ctx context.Context, id, token uuid.UUID, status, errorCode string, retryAt time.Time) error
	// MailStates is the latest mail of each of the Event's certificates,
	// by serial.
	MailStates(ctx context.Context, eventID uuid.UUID) (map[string]MailState, error)
}

// CertificateMailer sends one queued certificate mail and says what SkyNotify
// answered (*mail.SkyNotify). A Mailer without it is sent to directly, as
// before the queue.
type CertificateMailer interface {
	CertificateMail(ctx context.Context, idempotencyKey, recipientEmail, fullName string, vars map[string]string) error
}

// mailQueue is the queue and its sender, when both are there.
func (s *service) mailQueue() (MailQueue, CertificateMailer, bool) {
	if s.mails == nil {
		return nil, nil, false
	}
	sender, ok := s.mail.(CertificateMailer)
	if !ok || sender == nil {
		return nil, nil, false
	}
	return s.mails, sender, true
}

// queueMail writes c's issued mail into the queue; nothing without one.
// It runs before the job completes, so a mail is never lost to a crash
// between the two: the job's next claim queues it again, and the queue
// keeps one issued mail per certificate.
func (s *service) queueMail(ctx context.Context, c Certificate) error {
	queue, _, ok := s.mailQueue()
	if !ok {
		return nil
	}
	_, err := queue.EnqueueMail(ctx, Mail{ID: uuid.New(), CertificateID: c.ID, EventID: c.EventID, Reason: MailReasonIssued})
	return err
}

// sendMail sends c's mail directly, as before the queue; with the queue it
// does nothing (queueMail wrote it).
func (s *service) sendMail(ctx context.Context, c Certificate) {
	if _, _, ok := s.mailQueue(); ok {
		return
	}
	s.sendDirect(ctx, c)
}

// sendDirect posts c's mail once, inline, telling nothing back.
func (s *service) sendDirect(ctx context.Context, c Certificate) {
	if s.mail == nil {
		return
	}
	s.mail.Certificate(ctx, c.RecipientEmail, c.RecipientName, certificateMailVars(c))
}

func certificateMailVars(c Certificate) map[string]string {
	first := c.RecipientName
	if parts := strings.Fields(c.RecipientName); len(parts) > 0 {
		first = parts[0]
	}
	return map[string]string{
		"FirstName": first, "EventName": c.EventName, "VerifyURL": c.VerifyURL,
		// The template's "Düzenleyen" row: the organizer as the club shows it.
		"Serial": c.Serial, "OwnerTeam": organizer.Name(c.OwnerTeam),
	}
}

// ProcessMails sends up to limit due certificate mails. It returns how many
// it claimed; without a queue it does nothing.
func (s *service) ProcessMails(ctx context.Context, limit int) (int, error) {
	queue, sender, ok := s.mailQueue()
	if !ok {
		return 0, nil
	}
	claimed, err := queue.ClaimMails(ctx, limit, mailLeaseFor(limit))
	if err != nil {
		return 0, err
	}
	for _, m := range claimed {
		// A fresh lease for this send alone; a mail whose lease ran out
		// while the ones before it were sent is left to the next claim.
		if err := queue.RenewMailLease(ctx, m.ID, m.ClaimToken, mailRowBudget+time.Minute); err != nil {
			if !errors.Is(err, ErrLeaseLost) {
				log.Printf("ERROR certificate mail %s: lease not renewed, it waits for its lease to run out (%s)", m.ID, databaseErrorClass(err))
			}
			continue
		}
		status, code, retryAt := s.deliverMail(ctx, sender, m)
		if err := queue.FinishMail(ctx, m.ID, m.ClaimToken, status, code, retryAt); err != nil && !errors.Is(err, ErrLeaseLost) {
			log.Printf("ERROR certificate mail %s: not settled, it waits for its lease to run out (%s)", m.ID, databaseErrorClass(err))
		}
	}
	return len(claimed), nil
}

// deliverMail sends one claimed mail and says how it ends: sent, failed for
// good, cancelled, or queued again at retryAt. With the PDF on, the
// certificate's PDF goes along when it can (mailAttachments); a mail never
// waits or fails for want of it.
func (s *service) deliverMail(ctx context.Context, sender CertificateMailer, m Mail) (status, code string, retryAt time.Time) {
	now := time.Now().UTC()
	c := m.Certificate
	switch {
	case c.RevokedAt != nil:
		return MailCancelled, "revoked", now
	case strings.TrimSpace(c.RecipientEmail) == "":
		// Erased meanwhile, or a certificate with no address.
		return MailFailed, "no_recipient", now
	case now.After(m.FirstTriedAt.Add(MailKeyLimit)):
		// SkyNotify may have forgotten the key: a try could mail twice.
		return MailFailed, "expired", now
	}
	attachments := s.mailAttachments(ctx, sender, m)
	sendCtx, cancel := context.WithTimeout(ctx, mailSendBudget)
	defer cancel()
	err := s.postMail(sendCtx, sender, m, attachments)
	var refused *mail.SendError
	if attachments != nil && errors.As(err, &refused) && refused.Permanent() && strings.HasPrefix(refused.Code, "attachment.") {
		// SkyNotify refused the file (400 attachment.*): the verify link is
		// in the mail, so it goes without. A refused request leaves no key
		// behind, and the file is not part of the key's digest: the same key
		// is free. Any other refusal is the body's own and settles the mail
		// as before.
		s.mailPDF.skip(pdfSkipRefused)
		log.Printf("WARN certificate mail %s: SkyNotify refused the mail with the PDF (%d %s); sent without it", m.ID, refused.Status, refused.Code)
		err = s.postMail(sendCtx, sender, m, nil)
	}
	if err == nil {
		return MailSent, "", now
	}
	if errors.Is(err, mail.ErrTemplateUnconfigured) {
		return MailFailed, "mail_unconfigured", now
	}
	code = "skynotify_unreachable"
	if errors.As(err, &refused) {
		if refused.Status != 0 {
			code = "skynotify_" + strconv.Itoa(refused.Status)
		}
		if refused.Permanent() {
			return MailFailed, code, now
		}
	}
	next := now.Add(mailBackoff(m.Attempts))
	if next.After(m.FirstTriedAt.Add(MailRetryWindow)) {
		return MailFailed, code, now
	}
	return MailQueued, code, next
}

// mailBackoff is the wait after the attempts-th try: 1, 2, 4 … minutes, at
// most an hour.
func mailBackoff(attempts int) time.Duration {
	return min(time.Duration(1<<min(max(attempts-1, 0), 6))*time.Minute, time.Hour)
}

// ResendMail queues one certificate's mail again ("yeniden gönder"): for
// whoever may issue the Event's certificates, for an active certificate.
func (s *service) ResendMail(ctx context.Context, p authz.Principal, serial string) (MailResend, error) {
	queue, _, ok := s.mailQueue()
	if !ok {
		return MailResend{}, ErrInvalid
	}
	cert, _, err := s.store.GetBySerial(ctx, serial)
	if err != nil {
		return MailResend{}, err
	}
	ev, err := s.visibleEvent(ctx, p, cert.EventID)
	if err != nil {
		return MailResend{}, err
	}
	if !s.canIssue(p, ev.OwnerTeam) {
		return MailResend{}, ErrForbidden
	}
	if cert.RevokedAt != nil {
		return MailResend{}, ErrConflict
	}
	written, err := queue.EnqueueMail(ctx, Mail{ID: uuid.New(), CertificateID: cert.ID, EventID: cert.EventID, Reason: MailReasonResend, RequestedBy: principalUUIDPtr(p)})
	if err != nil {
		return MailResend{}, err
	}
	if !written {
		return MailResend{AlreadyQueued: 1}, nil
	}
	return MailResend{Queued: 1}, nil
}

// ResendEventMails queues the Event's certificate mails again: scope
// ResendFailed takes the active certificates whose latest mail failed,
// ResendAll every active one, at most once per ResendAllCooldown
// (ErrResendTooSoon). A certificate with a mail waiting is not queued
// twice.
func (s *service) ResendEventMails(ctx context.Context, p authz.Principal, eventID uuid.UUID, scope string) (MailResend, error) {
	queue, _, ok := s.mailQueue()
	if !ok || (scope != ResendFailed && scope != ResendAll) {
		return MailResend{}, ErrInvalid
	}
	ev, err := s.visibleEvent(ctx, p, eventID)
	if err != nil {
		return MailResend{}, err
	}
	if !s.canIssue(p, ev.OwnerTeam) {
		return MailResend{}, ErrForbidden
	}
	return queue.ResendEventMails(ctx, eventID, scope, principalUUIDPtr(p), ResendAllCooldown)
}

// mailStates is the Event's latest mail per certificate serial; empty
// without a queue.
func (s *service) mailStates(ctx context.Context, eventID uuid.UUID) (map[string]MailState, error) {
	queue, _, ok := s.mailQueue()
	if !ok {
		return map[string]MailState{}, nil
	}
	return queue.MailStates(ctx, eventID)
}

// MaintainMails sends queued certificate mails in the background until ctx
// is cancelled. The returned channel closes once the pass in flight has
// returned after that.
func MaintainMails(ctx context.Context, svc Service, interval time.Duration, limit int, onError func(error)) <-chan struct{} {
	if interval <= 0 {
		interval = 5 * time.Second
	}
	if limit <= 0 {
		limit = MailBatch
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				for {
					processed, err := svc.ProcessMails(ctx, limit)
					if err != nil {
						if onError != nil {
							onError(err)
						}
						break
					}
					if processed < limit {
						break
					}
				}
			}
		}
	}()
	return done
}
