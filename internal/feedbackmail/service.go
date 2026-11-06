// Package feedbackmail sends the feedback mails (etkinlik geri bildirim
// formu, ticket 06; docs/event-feedback.md): an Event's or Session's
// feedback form, with each person's own invite link, to whom checked in,
// two hours after it ends (one mail a person a day) or at once with
// "Şimdi gönder", and their one reminder three days later (ticket 07). It
// follows the Event status mail worker
// (internal/eventstatusmail): the queue holds ids, the address is read when
// the mail goes, a pass holds a mail under a lease and a failure is tried
// again later.
package feedbackmail

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"slices"
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
	// GiveUpAfter is how long after it was queued a mail is tried, whether
	// it was tried yet or not (a mail left over from a template key turned
	// off does not go weeks late).
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
	PlanReminders(ctx context.Context, now time.Time) (int, error)
	ClaimMail(ctx context.Context, now time.Time, lease time.Duration, kinds []string) (feedback.Mail, bool, error)
	Siblings(ctx context.Context, claimed feedback.Mail, now time.Time) ([]feedback.Mail, error)
	ClaimSibling(ctx context.Context, id uuid.UUID, now time.Time, lease time.Duration) (feedback.Mail, bool, error)
	PostponeMail(ctx context.Context, claimed feedback.Mail, at time.Time) error
	DayDue(ctx context.Context, ticketIDs []uuid.UUID, day uuid.UUID) (*time.Time, error)
	MailItems(ctx context.Context, mailID uuid.UUID) ([]feedback.MailItem, error)
	Invite(ctx context.Context, ticketID uuid.UUID, t feedback.Target, formID uuid.UUID) (feedback.Invite, error)
	ReminderInvite(ctx context.Context, ticketID uuid.UUID, t feedback.Target, now time.Time) (feedback.Invite, bool, error)
	MarkReminded(ctx context.Context, ids []uuid.UUID, at time.Time) error
	FinishMail(ctx context.Context, claimed feedback.Mail, at time.Time, sent bool) error
	RetryMail(ctx context.Context, claimed feedback.Mail, at time.Time) error
}

// Mailer posts one feedback mail (mail.SkyMail).
type Mailer interface {
	FeedbackConfigured(kind mail.FeedbackKind) bool
	Feedback(ctx context.Context, kind mail.FeedbackKind, idempotencyKey string, to mail.FeedbackRecipient, eventID uuid.UUID, eventName string, items []mail.FeedbackItem) error
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

// Report counts one pass: Planned targets and Reminders, and mails Sent
// (Reminded of them reminders), Dropped (no target left to send), Skipped
// (no address), Rejected by SkyMail, Expired, Off (no template key) and
// Retried later.
type Report struct {
	Planned, Reminders                                int
	Sent, Reminded                                    int
	Dropped, Skipped, Rejected, Expired, Off, Retried int
}

func (r Report) any() bool {
	return r.Planned+r.Reminders+r.Sent+r.Dropped+r.Skipped+r.Rejected+r.Expired+r.Off+r.Retried > 0
}

// Pass plans the due targets and reminders, then sends the due mails until
// none is due (or maxMails).
func (s *Service) Pass(ctx context.Context) (Report, error) {
	var report Report
	// A kind without a template key is off: nothing of it is planned, so no
	// target is used up and nothing piles up. Set again, the schedule takes
	// the targets still within feedback.AutoWindow and the reminders within
	// feedback.ReminderWindow, as after SkyMail was down. A mail queued
	// before the key went off expires GiveUpAfter after it was queued.
	var kinds []string
	if s.config.Mail.FeedbackConfigured(mail.FeedbackRequest) {
		kinds = append(kinds, feedback.KindRequest)
		planned, err := s.config.Queue.Plan(ctx, s.now().UTC())
		if err != nil {
			return report, fmt.Errorf("plan: %w", err)
		}
		report.Planned = planned
	}
	if s.config.Mail.FeedbackConfigured(mail.FeedbackReminder) {
		kinds = append(kinds, feedback.KindReminder)
		reminders, err := s.config.Queue.PlanReminders(ctx, s.now().UTC())
		if err != nil {
			return report, fmt.Errorf("plan reminders: %w", err)
		}
		report.Reminders = reminders
	}
	if len(kinds) == 0 {
		return report, nil
	}
	for range maxMails {
		claimed, ok, err := s.config.Queue.ClaimMail(ctx, s.now().UTC(), lease, kinds)
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
				if err = s.config.Queue.FinishMail(writeCtx, m, s.now().UTC(), out.sent); err != nil {
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
// sent is true when SkyMail took the mail.
type outcome struct {
	group    []feedback.Mail
	postpone *time.Time
	sent     bool
}

// recipient is a mail's Ticket's holder: a member's subject (uuid.Nil for
// a guest) and the address and names the mail reads.
type recipient struct {
	sub                uuid.UUID
	email, first, full string
}

// deliver sends the claimed mail, with the open mails of the same Event and
// day that go to the same address (a person with a member and a guest
// Ticket gets one mail; each Ticket keeps its own invite links). It waits
// when such a mail is not due yet. nil error when the group is done with
// (sent, or no longer to be sent), an error when a later try may pass.
func (s *Service) deliver(ctx context.Context, claimed feedback.Mail, report *Report) (outcome, error) {
	out := outcome{group: []feedback.Mail{claimed}}
	if s.now().Sub(claimed.CreatedAt) > GiveUpAfter {
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
	reminder := claimed.Kind == feedback.KindReminder
	if !reminder && !claimed.Manual && claimed.EventDayID != nil {
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
		if to.sub == uuid.Nil {
			// The address is a member's: their mail goes to their inbox too
			to.sub = r.sub
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
	var reminded []uuid.UUID
	eventName := ""
	for _, m := range out.group {
		items, err := s.config.Queue.MailItems(ctx, m.ID)
		if err != nil {
			return out, err
		}
		for _, it := range items {
			// A reminder goes on the schedule's terms whoever sent the form
			// (ticket 07: not with the box off).
			if !goes(it, m.Manual && !reminder) {
				continue
			}
			var inv feedback.Invite
			if reminder {
				var ok bool
				inv, ok, err = s.config.Queue.ReminderInvite(ctx, m.TicketID, it.Target, s.now().UTC())
				if err != nil {
					return out, err
				}
				if !ok {
					continue
				}
				reminded = append(reminded, inv.ID)
			} else {
				inv, err = s.config.Queue.Invite(ctx, m.TicketID, it.Target, it.FormID)
				if err != nil {
					return out, err
				}
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
	kind := mail.FeedbackRequest
	if reminder {
		kind = mail.FeedbackReminder
	}
	addressee := mail.FeedbackRecipient{Sub: to.sub, Email: to.email, FullName: to.full, FirstName: to.first}
	err = s.config.Mail.Feedback(ctx, kind, idempotencyKey(kind, out.group, addressee, eventName, links), addressee, claimed.EventID, eventName, links)
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
	out.sent = true
	if reminder {
		report.Reminded++
		writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), writeTimeout)
		defer cancel()
		if err := s.config.Queue.MarkReminded(writeCtx, reminded, s.now().UTC()); err != nil {
			// The mail went: it is finished all the same, and its request
			// is never reminded again.
			s.logger.Printf("feedback mail: a reminder went but its invites were not marked: %v", err)
		}
	}
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
	r := recipient{email: email, first: first, full: full}
	if t.TicketType != ticket.Guest && t.OwnerID != nil {
		r.sub = *t.OwnerID
	}
	return r, err == nil && email != "", err
}

// idempotencyKey is the same on every try of the same mail (its mails, its
// recipient and its links: the invite codes do not change), so a try after a
// lost answer is SkyMail's replay, never a second Notification. A mail whose
// links changed meanwhile (a target dropped) is another body, and SkyMail
// refuses another body under a key it has seen: it gets a key of its own.
func idempotencyKey(kind mail.FeedbackKind, group []feedback.Mail, to mail.FeedbackRecipient, eventName string, links []mail.FeedbackItem) string {
	ids := make([]string, 0, len(group))
	for _, m := range group {
		ids = append(ids, m.ID.String())
	}
	slices.Sort(ids)
	body, _ := json.Marshal(struct {
		Mails     []string
		Sub       uuid.UUID
		Email     string
		EventName string
		Items     []mail.FeedbackItem
	}{ids, to.Sub, to.Email, eventName, links})
	sum := sha256.Sum256(body)
	return "core:feedback-" + string(kind) + ":" + hex.EncodeToString(sum[:16])
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
				logf("feedback mail: %d targets and %d reminders planned; %d sent (%d reminders), %d dropped, %d skipped, %d refused by SkyMail, %d expired, %d retried later",
					report.Planned, report.Reminders, report.Sent, report.Reminded, report.Dropped, report.Skipped, report.Rejected, report.Expired, report.Retried)
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
