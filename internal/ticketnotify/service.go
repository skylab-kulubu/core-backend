// Package ticketnotify tells a new Ticket's holder that the Ticket is there
// (iletişim-servisi 06, ADR 0070; docs/ticket-created-notification.md):
// core says what happened and to whom, SkyMail picks the channels (inbox and
// e-mail) and the text. Member apply, Apply-for-other and Guest apply queue
// the Ticket in the statement that writes it (ticket_notifications); this
// worker reads the holder when it sends, posts the Notification under a key
// of the Ticket, so a retry never tells anyone twice, and removes the row.
package ticketnotify

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/mail"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

const (
	// RetryFirst is the wait before a Notification that failed is tried
	// again; it doubles with each failure in a row, up to retryMax.
	RetryFirst = 30 * time.Second
	retryMax   = 30 * time.Minute
	// GiveUpAfter is how long a Notification is tried, and sent at all: a
	// ticket mail a day late is no longer news.
	GiveUpAfter = 24 * time.Hour
	// lease is how long a pass holds the rows it took.
	lease        = 2 * time.Minute
	pollInterval = 10 * time.Second
	// batch bounds one claim.
	batch        = 50
	writeTimeout = 5 * time.Second
	timeZone     = "Europe/Istanbul"
	dateLayout   = "02.01.2006 15:04"
	// keyVersion is the version in the Idempotency-Key: a later change of
	// what one Ticket's Notification says takes a new one.
	keyVersion = "v1"
)

// Queued is one Ticket waiting for its Notification.
type Queued struct {
	TicketID  uuid.UUID
	CreatedAt time.Time
	Attempts  int
	// ClaimedAt is the claim that returned the row: Complete and Retry write
	// only while the row still carries it.
	ClaimedAt time.Time
}

// Queue keeps the Tickets waiting (PostgresQueue: ticket_notifications).
type Queue interface {
	// Claim takes up to limit due rows, oldest first, and holds them for
	// lease.
	Claim(ctx context.Context, now time.Time, lease time.Duration, limit int) ([]Queued, error)
	// Complete removes a claimed row whose Notification is done with.
	Complete(ctx context.Context, claimed Queued) error
	// Retry counts a failure, makes the row due at at and releases it.
	Retry(ctx context.Context, claimed Queued, at time.Time) error
}

// Mailer posts the Notification (mail.SkyMail).
type Mailer interface {
	TicketCreated(ctx context.Context, idempotencyKey string, holder mail.TicketHolder, vars map[string]string, source mail.NotificationSource, expiresAt *time.Time) error
}

// Tickets, Events and Users are what the worker reads when it sends.
type Tickets interface {
	Get(ctx context.Context, id uuid.UUID) (ticket.Ticket, error)
}

type Events interface {
	GetIncludingArchived(ctx context.Context, id uuid.UUID) (event.Event, error)
}

type Users interface {
	Get(ctx context.Context, id uuid.UUID) (user.User, error)
}

// Config is what a Service takes.
type Config struct {
	Queue   Queue
	Tickets Tickets
	Events  Events
	Users   Users
	Mail    Mailer
	// Now is the clock; nil is time.Now.
	Now func() time.Time
	// Logger hears counts and failures, never a person; nil is log.Default.
	Logger *log.Logger
}

// Service sends the queued Notifications (Run).
type Service struct {
	config   Config
	now      func() time.Time
	logger   *log.Logger
	location *time.Location
}

// NewService checks config.
func NewService(config Config) (*Service, error) {
	if config.Queue == nil || config.Tickets == nil || config.Events == nil || config.Users == nil || config.Mail == nil {
		return nil, errors.New("ticketnotify: a queue, the Tickets, Events and users and a mailer are needed")
	}
	location, err := time.LoadLocation(timeZone)
	if err != nil {
		return nil, fmt.Errorf("ticketnotify: %w", err)
	}
	now := config.Now
	if now == nil {
		now = time.Now
	}
	logger := config.Logger
	if logger == nil {
		logger = log.Default()
	}
	return &Service{config: config, now: now, logger: logger, location: location}, nil
}

// Report counts one pass.
type Report struct {
	// Sent: SkyMail took the Notification. Dropped: nothing to tell any
	// more (the Ticket, its Event or its holder is gone, the Event is not
	// published, or is over). Rejected: SkyMail refused the request itself.
	// Expired: tried for GiveUpAfter. Retried: tried again later.
	Sent, Dropped, Rejected, Expired, Retried int
}

func (r Report) any() bool {
	return r.Sent+r.Dropped+r.Rejected+r.Expired+r.Retried > 0
}

// IdempotencyKey is the key of a Ticket's Notification: the type, the
// Ticket and the version, the same on every try.
func IdempotencyKey(ticketID uuid.UUID) string {
	return "core:" + mail.TicketCreatedType + ":" + ticketID.String() + ":" + keyVersion
}

// Pass sends the due Notifications of one claim.
func (s *Service) Pass(ctx context.Context) (Report, error) {
	var report Report
	claimed, err := s.config.Queue.Claim(ctx, s.now().UTC(), lease, batch)
	if err != nil {
		return report, err
	}
	for _, q := range claimed {
		stale := s.now().Sub(q.CreatedAt) > GiveUpAfter
		var (
			outcome outcome
			err     error
		)
		if !stale {
			outcome, err = s.deliver(ctx, q)
		}
		writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), writeTimeout)
		switch {
		case stale:
			// A day late is no longer news, whether it failed until now or
			// waited while the worker was off.
			report.Expired++
			err = s.config.Queue.Complete(writeCtx, q)
		case err != nil && ctx.Err() != nil:
			// Core is stopping: the row is taken again once its lease runs
			// out, under the same key.
			cancel()
			return report, ctx.Err()
		case err != nil:
			report.Retried++
			s.logger.Printf("ticket notification: failed and is tried again: %v", err)
			err = s.config.Queue.Retry(writeCtx, q, s.now().UTC().Add(backoff(q.Attempts+1)))
		default:
			switch outcome {
			case sent:
				report.Sent++
			case dropped:
				report.Dropped++
			case rejected:
				report.Rejected++
			}
			err = s.config.Queue.Complete(writeCtx, q)
		}
		cancel()
		if err != nil {
			return report, err
		}
	}
	return report, nil
}

type outcome int

const (
	sent outcome = iota
	dropped
	rejected
)

// deliver posts q's Notification; an error when a later try may pass.
func (s *Service) deliver(ctx context.Context, q Queued) (outcome, error) {
	t, err := s.config.Tickets.Get(ctx, q.TicketID)
	if errors.Is(err, ticket.ErrNotFound) {
		return dropped, nil
	}
	if err != nil {
		return 0, err
	}
	ev, err := s.config.Events.GetIncludingArchived(ctx, t.EventID)
	if errors.Is(err, event.ErrNotFound) {
		return dropped, nil
	}
	if err != nil {
		return 0, err
	}
	// Only a published Event that is not over is told of: a draft's
	// Tickets (speakers, the team, added from the panel) and a cancelled
	// or archived Event's are not news to anyone.
	if ev.ArchivedAt != nil || ev.Status != event.StatusPublished || (ev.EndDate != nil && !ev.EndDate.After(s.now())) {
		return dropped, nil
	}
	holder, ok, err := s.holder(ctx, t)
	if err != nil {
		return 0, err
	}
	if !ok {
		return dropped, nil
	}
	err = s.config.Mail.TicketCreated(ctx, IdempotencyKey(t.ID), holder, s.vars(ev), mail.NotificationSource{Kind: "ticket", Ref: t.ID.String()}, ev.EndDate)
	var refused *mail.SendError
	switch {
	case errors.As(err, &refused) && refused.Permanent():
		// SkyMail refuses this body itself (400, 422): the same body would be
		// refused again.
		return rejected, nil
	case err != nil:
		return 0, err
	}
	return sent, nil
}

// holder is whom t's Notification goes to: a registered holder's subject
// while their account is active, a guest's address. Not ok when nobody may
// be written to.
func (s *Service) holder(ctx context.Context, t ticket.Ticket) (mail.TicketHolder, bool, error) {
	if t.TicketType == ticket.Guest {
		email := strings.ToLower(strings.TrimSpace(t.GuestEmail))
		if email == "" {
			return mail.TicketHolder{}, false, nil
		}
		return mail.TicketHolder{Email: email, FullName: joinName(t.GuestFirstName, t.GuestLastName)}, true, nil
	}
	if t.OwnerID == nil || *t.OwnerID == user.DeletedSubject {
		return mail.TicketHolder{}, false, nil
	}
	u, err := s.config.Users.Get(ctx, *t.OwnerID)
	switch {
	case errors.Is(err, user.ErrNotFound), errors.Is(err, user.ErrAccountBlocked):
		return mail.TicketHolder{}, false, nil
	case err != nil:
		return mail.TicketHolder{}, false, err
	case u.AccountState != user.AccountActive:
		return mail.TicketHolder{}, false, nil
	}
	return mail.TicketHolder{Sub: *t.OwnerID}, true, nil
}

// vars are the type's variables (SkyMail emails/event-ticket-created.tsx):
// the Event's, the same for any holder. EventId fills the inbox item's link.
// Every key is always there, EventDate empty without a start: SkyMail
// renders the inbox text with missingkey=error.
func (s *Service) vars(ev event.Event) map[string]string {
	vars := map[string]string{
		"EventId":   ev.ID.String(),
		"EventName": ev.Name,
		"EventDate": "",
		"Venue":     ev.Location,
		"OwnerTeam": ev.OwnerTeam,
	}
	if ev.StartDate != nil {
		vars["EventDate"] = ev.StartDate.In(s.location).Format(dateLayout)
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

// Run sends the queued Notifications until ctx is cancelled; the channel
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
				logf("ticket notification: queue: %v", err)
			case report.any():
				logf("ticket notification: %d sent, %d dropped, %d refused by SkyMail, %d given up, %d tried again later",
					report.Sent, report.Dropped, report.Rejected, report.Expired, report.Retried)
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
