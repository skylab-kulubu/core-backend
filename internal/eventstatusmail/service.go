// Package eventstatusmail sends the Event status mails (ADR 0066 addendum
// 2026-10-19, decision 1; ticket 14): an Event's Ticket holders are told of
// its cancellation when the organizer asks for it, and told again that it
// is back when the cancellation is undone. The cancellation and its undoing
// queue the mail in their own write (event.StatusMailQueue); this worker
// reads the recipients when it sends, and records each Ticket a mail
// reached, so that nobody gets the same mail twice.
package eventstatusmail

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/mail"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

const (
	// RetryFirst is the wait before a mail that failed is tried again; it
	// doubles with each failure in a row, up to retryMax.
	RetryFirst = 30 * time.Second
	retryMax   = 30 * time.Minute
	// GiveUpAfter is how long a mail is tried: a cancellation mail days
	// late is no longer news.
	GiveUpAfter = 72 * time.Hour
	// lease is how long a pass holds the mail it took; one mail is sent to
	// every recipient in it, so the lease outlasts that.
	lease        = 10 * time.Minute
	pollInterval = 15 * time.Second
	// maxMails bounds one pass.
	maxMails = 20
	// renewEvery is how many sends a pass makes between two renewals of
	// its claim.
	renewEvery   = 20
	writeTimeout = 5 * time.Second
	timeZone     = "Europe/Istanbul"
	dateLayout   = "02.01.2006 15:04"
)

// Mailer posts one status mail to one recipient (mail.SkyMail).
type Mailer interface {
	EventStatusConfigured(kind mail.EventStatusKind) bool
	EventStatus(ctx context.Context, kind mail.EventStatusKind, recipient, fullName string, vars map[string]string) error
}

// Config is what a Service takes.
type Config struct {
	Queue   event.StatusMailQueue
	Events  event.Store
	Tickets ticket.Store
	Users   user.Store
	Mail    Mailer
	// Now is the clock; nil is time.Now.
	Now func() time.Time
	// Logger hears counts and failures, never a person; nil is log.Default.
	Logger *log.Logger
}

// Service sends the queued status mails (Run).
type Service struct {
	config   Config
	now      func() time.Time
	logger   *log.Logger
	location *time.Location

	mu       sync.Mutex
	outcomes map[string]int64
}

// NewService checks config.
func NewService(config Config) (*Service, error) {
	if config.Queue == nil || config.Events == nil || config.Tickets == nil || config.Users == nil || config.Mail == nil {
		return nil, errors.New("eventstatusmail: a queue, the Events, Tickets and users and a mailer are needed")
	}
	location, err := time.LoadLocation(timeZone)
	if err != nil {
		return nil, fmt.Errorf("eventstatusmail: %w", err)
	}
	now := config.Now
	if now == nil {
		now = time.Now
	}
	logger := config.Logger
	if logger == nil {
		logger = log.Default()
	}
	return &Service{config: config, now: now, logger: logger, location: location, outcomes: map[string]int64{}}, nil
}

// Report counts one pass: Mails are the queued mails done with, the rest
// are mails to one recipient.
type Report struct {
	Mails, Dropped, Expired, Off, Retried int
	Sent, Skipped, Rejected               int
}

func (r Report) any() bool {
	return r.Mails+r.Dropped+r.Expired+r.Off+r.Retried+r.Sent+r.Skipped+r.Rejected > 0
}

// Pass sends the due mails until none is due (or maxMails).
func (s *Service) Pass(ctx context.Context) (Report, error) {
	var report Report
	for range maxMails {
		claimed, ok, err := s.config.Queue.ClaimStatusMail(ctx, s.now().UTC(), lease)
		if err != nil || !ok {
			return report, err
		}
		finished, err := s.deliver(ctx, claimed, &report)
		writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), writeTimeout)
		switch {
		case err != nil && ctx.Err() != nil:
			// Core is stopping: the mail is taken again once its lease
			// runs out; what it reached is recorded.
			cancel()
			return report, ctx.Err()
		case err != nil:
			report.Retried++
			s.logger.Printf("event status mail: %s mail of an Event failed and is tried again: %v", claimed.Kind, err)
			err = s.config.Queue.RetryStatusMail(writeCtx, claimed, s.now().UTC().Add(backoff(claimed.Attempts+1)))
		case finished:
			report.Mails++
			err = s.config.Queue.FinishStatusMail(writeCtx, claimed, s.now().UTC())
		}
		cancel()
		if err != nil {
			return report, err
		}
	}
	return report, nil
}

// deliver sends claimed to each of its recipients it has not reached: true
// when the mail is done with (sent, or no longer to be sent), an error when
// a send failed in a way a later try may pass.
func (s *Service) deliver(ctx context.Context, claimed event.StatusMail, report *Report) (bool, error) {
	ev, err := s.config.Events.GetIncludingArchived(ctx, claimed.EventID)
	if errors.Is(err, event.ErrNotFound) {
		report.Dropped++
		return true, nil
	}
	if err != nil {
		return false, err
	}
	kind := mail.EventStatusKind(claimed.Kind)
	current, err := s.current(ctx, claimed, ev)
	if err != nil {
		return false, err
	}
	switch {
	case !current:
		report.Dropped++
		return true, nil
	case !s.config.Mail.EventStatusConfigured(kind):
		report.Off++
		return true, nil
	case s.now().Sub(claimed.CreatedAt) > GiveUpAfter:
		report.Expired++
		return true, nil
	}
	recipients, err := s.recipients(ctx, claimed)
	if err != nil {
		return false, err
	}
	vars := s.vars(ev, claimed.Kind)
	sends := 0
	for _, r := range recipients {
		if r.sent {
			continue
		}
		if r.email == "" {
			report.Skipped++
			continue
		}
		// The Event is read again before each send, and the claim renewed
		// every renewEvery sends: a cancellation undone (or made again)
		// while a long mail goes stops it where it is.
		if sends > 0 && sends%renewEvery == 0 {
			if err := s.config.Queue.RenewStatusMail(ctx, claimed, s.now().UTC().Add(lease)); err != nil {
				return false, err
			}
		}
		if ev, err = s.config.Events.GetIncludingArchived(ctx, claimed.EventID); err != nil {
			return false, err
		}
		if current, err = s.current(ctx, claimed, ev); err != nil {
			return false, err
		}
		if !current {
			report.Dropped++
			return true, nil
		}
		sends++
		mailVars := make(map[string]string, len(vars)+1)
		for k, v := range vars {
			mailVars[k] = v
		}
		mailVars["FirstName"] = r.firstName
		err := s.config.Mail.EventStatus(ctx, kind, r.email, r.fullName, mailVars)
		var refused *mail.SendError
		switch {
		case errors.As(err, &refused) && refused.Permanent():
			// SkyMail refuses this one (an address it does not take): the
			// same body would be refused again.
			report.Rejected++
			s.count("rejected")
			continue
		case err != nil:
			return false, err
		}
		report.Sent++
		s.count("sent")
		for _, id := range r.tickets {
			if err := s.config.Queue.RecordStatusMailSend(ctx, claimed.ID, id); err != nil {
				return false, err
			}
		}
	}
	return true, nil
}

// current reports whether claimed still tells what the Event is: a
// cancellation mail only while the Event is cancelled by the cancellation
// that queued it (not by a later one, after an undo: that one has its own
// mail, or none), a reinstated mail only while the Event is published and
// no later cancellation was mailed.
func (s *Service) current(ctx context.Context, claimed event.StatusMail, ev event.Event) (bool, error) {
	if ev.ArchivedAt != nil {
		return false, nil
	}
	switch claimed.Kind {
	case event.StatusMailCancelled:
		// The row and cancelled_at take the same now() in one transaction.
		return ev.Status == event.StatusCancelled && ev.CancelledAt != nil && !claimed.CreatedAt.Before(*ev.CancelledAt), nil
	case event.StatusMailReinstated:
		if ev.Status != event.StatusPublished {
			return false, nil
		}
		newer, err := s.config.Queue.NewerCancellationMail(ctx, claimed.EventID, claimed.CreatedAt)
		return !newer, err
	default:
		return false, nil
	}
}

// recipient is one address a mail goes to, with every Ticket of the Event
// under it (a person may hold a registered and a guest Ticket).
type recipient struct {
	email, firstName, fullName string
	tickets                    []uuid.UUID
	sent                       bool
}

// recipients are the Ticket holders of the mail, one per address: every
// Ticket of the Event for a cancellation mail; for a reinstated mail only
// the Tickets the cancellation mail it follows reached. A Ticket the mail
// reached already counts as sent. A registered holder whose account is
// erased, being erased or disabled, or who has no primary e-mail, gets
// none; nor does a guest without an address.
func (s *Service) recipients(ctx context.Context, claimed event.StatusMail) ([]*recipient, error) {
	tickets, err := s.config.Tickets.ListByEvent(ctx, claimed.EventID)
	if err != nil {
		return nil, err
	}
	reached := map[uuid.UUID]bool{}
	sends, err := s.config.Queue.StatusMailSends(ctx, claimed.ID)
	if err != nil {
		return nil, err
	}
	for _, id := range sends {
		reached[id] = true
	}
	var allowed map[uuid.UUID]bool
	if claimed.Kind == event.StatusMailReinstated {
		allowed = map[uuid.UUID]bool{}
		if claimed.Follows != nil {
			followed, err := s.config.Queue.StatusMailSends(ctx, *claimed.Follows)
			if err != nil {
				return nil, err
			}
			for _, id := range followed {
				allowed[id] = true
			}
		}
	}
	byEmail := map[string]*recipient{}
	var out []*recipient
	for _, t := range tickets {
		if allowed != nil && !allowed[t.ID] {
			continue
		}
		email, first, full, err := s.holder(ctx, t)
		if err != nil {
			return nil, err
		}
		key := email
		r := byEmail[key]
		if r == nil || email == "" {
			r = &recipient{email: email, firstName: first, fullName: full}
			out = append(out, r)
			if email != "" {
				byEmail[key] = r
			}
		}
		r.tickets = append(r.tickets, t.ID)
		r.sent = r.sent || reached[t.ID]
	}
	return out, nil
}

// holder is the Ticket's holder as a mail names them; no address when they
// may not be written to.
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

// vars are the mail's variables but FirstName: the template's contract.
func (s *Service) vars(ev event.Event, kind event.StatusMailKind) map[string]string {
	vars := map[string]string{"EventName": ev.Name, "EventStart": ""}
	if ev.StartDate != nil {
		vars["EventStart"] = ev.StartDate.In(s.location).Format(dateLayout)
	}
	if kind == event.StatusMailCancelled {
		vars["CancelReason"] = ev.CancelReason
	}
	return vars
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

func (s *Service) count(outcome string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.outcomes[outcome]++
}

// Run sends the queued mails until ctx is cancelled; the channel closes once
// it has stopped.
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
				logf("event status mail: queue: %v", err)
			case report.any():
				logf("event status mail: %d mails done (%d dropped, %d expired, %d off), %d retried later; %d sent, %d skipped, %d refused by SkyMail",
					report.Mails, report.Dropped, report.Expired, report.Off, report.Retried, report.Sent, report.Skipped, report.Rejected)
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
