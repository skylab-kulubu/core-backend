// Package feedbackmail sends the feedback mails (etkinlik geri bildirim
// formu, ticket 06; docs/event-feedback.md): an Event's or Session's
// feedback form, with each person's own invite link, to whom checked in,
// two hours after it ends (one mail a person a day) or at once with
// "Şimdi gönder". It follows the Event status mail worker
// (internal/eventstatusmail): the queue holds ids, the address is read when
// the mail goes, a pass holds a mail under a lease and a failure is tried
// again later.
package feedbackmail

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/feedback"
	"github.com/skylab-kulubu/core-backend/internal/mail"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

const (
	// RetryFirst is the wait before a mail that failed is tried again; it
	// doubles with each failure in a row, up to retryMax.
	RetryFirst = 30 * time.Second
	retryMax   = 30 * time.Minute
	// GiveUpAfter is how long past its due time a mail is tried.
	GiveUpAfter = 72 * time.Hour
	lease       = 5 * time.Minute
	// pollInterval is the schedule's minute (ticket 06).
	pollInterval = time.Minute
	maxMails     = 50
	writeTimeout = 5 * time.Second
)

// Queue is the feedback mail queue (feedback.PostgresStore).
type Queue interface {
	Plan(ctx context.Context, now time.Time) (int, error)
	ClaimMail(ctx context.Context, now time.Time, lease time.Duration) (feedback.Mail, bool, error)
	Siblings(ctx context.Context, claimed feedback.Mail, now time.Time) ([]feedback.Mail, error)
	ClaimSibling(ctx context.Context, id uuid.UUID, now time.Time, lease time.Duration) (feedback.Mail, bool, error)
	PostponeMail(ctx context.Context, claimed feedback.Mail, at time.Time) error
	DayDue(ctx context.Context, ticketIDs []uuid.UUID, day uuid.UUID) (*time.Time, error)
	MailItems(ctx context.Context, mailID uuid.UUID) ([]feedback.MailItem, error)
	Invite(ctx context.Context, ticketID uuid.UUID, t feedback.Target, formID uuid.UUID) (feedback.Invite, error)
	FinishMail(ctx context.Context, claimed feedback.Mail, at time.Time) error
	RetryMail(ctx context.Context, claimed feedback.Mail, at time.Time) error
}

// Mailer posts one feedback mail (mail.SkyMail).
type Mailer interface {
	FeedbackConfigured(kind mail.FeedbackKind) bool
	Feedback(ctx context.Context, kind mail.FeedbackKind, recipient, fullName, firstName, eventName string, items []mail.FeedbackItem) error
}

// Config is what a Service takes.
type Config struct {
	Queue   Queue
	Tickets ticket.Store
	Users   user.Store
	Mail    Mailer
	// Now is the clock; nil is time.Now.
	Now func() time.Time
	// Logger hears counts and failures, never a person; nil is log.Default.
	Logger *log.Logger
}

// Service plans and sends the feedback mails (Run).
type Service struct {
	config Config
	now    func() time.Time
	logger *log.Logger
}

func NewService(config Config) (*Service, error) {
	if config.Queue == nil || config.Tickets == nil || config.Users == nil || config.Mail == nil {
		return nil, errors.New("feedbackmail: a queue, the Tickets and users and a mailer are needed")
	}
	now := config.Now
	if now == nil {
		now = time.Now
	}
	logger := config.Logger
	if logger == nil {
		logger = log.Default()
	}
	return &Service{config: config, now: now, logger: logger}, nil
}

// Report counts one pass: Planned targets, and mails Sent, Dropped (no
// target left to send), Skipped (no address), Rejected by SkyMail,
// Expired, Off (no template key) and Retried later.
type Report struct {
	Planned                                                 int
	Sent, Dropped, Skipped, Rejected, Expired, Off, Retried int
}

func (r Report) any() bool {
	return r.Planned+r.Sent+r.Dropped+r.Skipped+r.Rejected+r.Expired+r.Off+r.Retried > 0
}

// Pass plans the due targets, then sends the due mails until none is due
// (or maxMails).
func (s *Service) Pass(ctx context.Context) (Report, error) {
	var report Report
	planned, err := s.config.Queue.Plan(ctx, s.now().UTC())
	if err != nil {
		return report, fmt.Errorf("plan: %w", err)
	}
	report.Planned = planned
	if !s.config.Mail.FeedbackConfigured(mail.FeedbackRequest) {
		// Off: the mails wait in the queue until a template key is set.
		return report, nil
	}
	for range maxMails {
		claimed, ok, err := s.config.Queue.ClaimMail(ctx, s.now().UTC(), lease)
		if err != nil || !ok {
			return report, err
		}
		out, err := s.deliver(ctx, claimed, &report)
		writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), writeTimeout)
		switch {
		case err != nil && ctx.Err() != nil:
			cancel()
			return report, ctx.Err()
		case out.postpone != nil:
			err = s.config.Queue.PostponeMail(writeCtx, claimed, *out.postpone)
		case err != nil:
			report.Retried++
			s.logger.Printf("feedback mail: a mail failed and is tried again: %v", err)
			at := s.now().UTC().Add(backoff(claimed.Attempts + 1))
			for _, m := range out.group {
				if err = s.config.Queue.RetryMail(writeCtx, m, at); err != nil {
					break
				}
			}
		default:
			for _, m := range out.group {
				if err = s.config.Queue.FinishMail(writeCtx, m, s.now().UTC()); err != nil {
					break
				}
			}
		}
		cancel()
		if err != nil {
			return report, err
		}
	}
	return report, nil
}

// outcome is what became of a claimed mail: the mails sent (or dropped)
// together with it, all finished or all retried; or a time to wait for.
type outcome struct {
	group    []feedback.Mail
	postpone *time.Time
}

// recipient is a mail's Ticket's holder.
type recipient struct {
	email, first, full string
}

// deliver sends the claimed mail, with the open mails of the same Event and
// day that go to the same address (a person with a member and a guest
// Ticket gets one mail; each Ticket keeps its own invite links). It waits
// when such a mail is not due yet. nil error when the group is done with
// (sent, or no longer to be sent), an error when a later try may pass.
func (s *Service) deliver(ctx context.Context, claimed feedback.Mail, report *Report) (outcome, error) {
	out := outcome{group: []feedback.Mail{claimed}}
	if s.now().Sub(claimed.CreatedAt) > GiveUpAfter && claimed.Attempts > 0 {
		report.Expired++
		return out, nil
	}
	to, ok, err := s.recipientOf(ctx, claimed.TicketID)
	if err != nil {
		return out, err
	}
	if !ok {
		report.Skipped++
		return out, nil
	}
	if !claimed.Manual && claimed.EventDayID != nil {
		// The address's day ends with the last Session any of its Tickets
		// checked in to: wait for it.
		due, err := s.addressDayDue(ctx, claimed, to.email)
		if err != nil {
			return out, err
		}
		if due != nil && due.After(s.now()) {
			out.postpone = due
			return out, nil
		}
	}
	siblings, err := s.config.Queue.Siblings(ctx, claimed, s.now().UTC())
	if err != nil {
		return out, err
	}
	var same []feedback.Mail
	for _, m := range siblings {
		r, ok, err := s.recipientOf(ctx, m.TicketID)
		if err != nil {
			return out, err
		}
		if !ok || r.email != to.email {
			continue
		}
		if m.NextAttemptAt.After(s.now()) && m.ClaimedAt.IsZero() {
			// One mail, when the last of them is due.
			if out.postpone == nil || m.NextAttemptAt.After(*out.postpone) {
				at := m.NextAttemptAt
				out.postpone = &at
			}
			continue
		}
		same = append(same, m)
	}
	if out.postpone != nil {
		return out, nil
	}
	for _, m := range same {
		taken, ok, err := s.config.Queue.ClaimSibling(ctx, m.ID, s.now().UTC(), lease)
		if err != nil {
			return out, err
		}
		if ok {
			out.group = append(out.group, taken)
		}
	}
	var links []mail.FeedbackItem
	eventName := ""
	for _, m := range out.group {
		items, err := s.config.Queue.MailItems(ctx, m.ID)
		if err != nil {
			return out, err
		}
		for _, it := range items {
			if !goes(it, m.Manual) {
				continue
			}
			inv, err := s.config.Queue.Invite(ctx, m.TicketID, it.Target, it.FormID)
			if err != nil {
				return out, err
			}
			link, err := feedback.Link(it.FormURL, inv.Code)
			if err != nil {
				continue
			}
			title := it.SessionTitle
			if it.SessionID == nil {
				title = it.EventName
			}
			eventName = it.EventName
			links = append(links, mail.FeedbackItem{Title: title, URL: link})
		}
	}
	if len(links) == 0 {
		report.Dropped++
		return out, nil
	}
	err = s.config.Mail.Feedback(ctx, mail.FeedbackRequest, to.email, to.full, to.first, eventName, links)
	var refused *mail.SendError
	switch {
	case errors.Is(err, mail.ErrTemplateUnconfigured):
		report.Off++
		return out, errors.New("feedback mail: template not configured")
	case errors.As(err, &refused) && refused.Permanent():
		report.Rejected++
		return out, nil
	case err != nil:
		return out, err
	}
	report.Sent++
	return out, nil
}

// addressDayDue is when the day's mail of every Ticket of the Event held
// under email goes (feedback.PostgresStore.DayDue).
func (s *Service) addressDayDue(ctx context.Context, claimed feedback.Mail, email string) (*time.Time, error) {
	tickets, err := s.config.Tickets.ListByEvent(ctx, claimed.EventID)
	if err != nil {
		return nil, err
	}
	ids := []uuid.UUID{claimed.TicketID}
	for _, t := range tickets {
		if t.ID == claimed.TicketID {
			continue
		}
		holder, _, _, err := s.holder(ctx, t)
		if err != nil {
			return nil, err
		}
		if holder == email {
			ids = append(ids, t.ID)
		}
	}
	return s.config.Queue.DayDue(ctx, ids, *claimed.EventDayID)
}

// recipientOf is the Ticket's holder; false when there is nobody to write
// to (no Ticket, an erased or blocked account, no address).
func (s *Service) recipientOf(ctx context.Context, ticketID uuid.UUID) (recipient, bool, error) {
	t, err := s.config.Tickets.Get(ctx, ticketID)
	if errors.Is(err, ticket.ErrNotFound) {
		return recipient{}, false, nil
	}
	if err != nil {
		return recipient{}, false, err
	}
	email, first, full, err := s.holder(ctx, t)
	return recipient{email: email, first: first, full: full}, err == nil && email != "", err
}

// goes reports whether the target still goes: its Event published and
// current, its Session (if any) neither cancelled nor archived, the form the
// same as when it was queued; and, for the schedule's mail, its box still on
// and a Session's form only while the mode is per_session.
func goes(it feedback.MailItem, manual bool) bool {
	if it.EventArchived || it.EventStatus != string(event.StatusPublished) || it.SessionGone {
		return false
	}
	if id, ok := event.FormIDFromURL(it.FormURL); !ok || id != it.FormID {
		return false
	}
	if manual {
		return true
	}
	if !it.AutoSend {
		return false
	}
	return it.SessionID == nil || it.FeedbackMode == string(event.FeedbackPerSession)
}

// holder is the Ticket's holder as the mail names them; no address when they
// may not be written to (as the Event status mails decide it).
func (s *Service) holder(ctx context.Context, t ticket.Ticket) (email, first, full string, err error) {
	if t.TicketType == ticket.Guest {
		first = strings.TrimSpace(t.GuestFirstName)
		return strings.ToLower(strings.TrimSpace(t.GuestEmail)), first, joinName(first, t.GuestLastName), nil
	}
	if t.OwnerID == nil || *t.OwnerID == user.DeletedSubject {
		return "", "", "", nil
	}
	u, err := s.config.Users.Get(ctx, *t.OwnerID)
	switch {
	case errors.Is(err, user.ErrNotFound), errors.Is(err, user.ErrAccountBlocked):
		return "", "", "", nil
	case err != nil:
		return "", "", "", err
	case u.AccountState != user.AccountActive:
		return "", "", "", nil
	}
	first = strings.TrimSpace(u.FirstName)
	return strings.ToLower(strings.TrimSpace(u.Email)), first, joinName(first, u.LastName), nil
}

func joinName(first, last string) string {
	return strings.TrimSpace(strings.TrimSpace(first) + " " + strings.TrimSpace(last))
}

func backoff(failures int) time.Duration {
	wait := RetryFirst
	for i := 1; i < failures && wait < retryMax; i++ {
		wait *= 2
	}
	return min(wait, retryMax)
}

// Run plans and sends every minute until ctx is cancelled; the channel
// closes once it has stopped.
func (s *Service) Run(ctx context.Context, logf func(string, ...any)) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(pollInterval)
		defer ticker.Stop()
		for {
			report, err := s.Pass(ctx)
			switch {
			case err != nil && ctx.Err() == nil:
				logf("feedback mail: %v", err)
			case report.any():
				logf("feedback mail: %d targets planned; %d sent, %d dropped, %d skipped, %d refused by SkyMail, %d expired, %d retried later",
					report.Planned, report.Sent, report.Dropped, report.Skipped, report.Rejected, report.Expired, report.Retried)
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	return done
}
