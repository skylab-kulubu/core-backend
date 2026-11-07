// Package eventupdatenotify tells an Event's Ticket holders that its time or
// place changed (iletişim-servisi 06, ADR 0070;
// docs/event-updated-notification.md): core says what happened and to whom,
// SkyMail picks the channels (inbox and e-mail) and the text. The Event's
// update queues the change in its own transaction
// (event_update_notifications) and gathers the changes of
// event.UpdateNotificationWindow in one row; this worker reads the Event and
// its holders when it sends, tells each holder once, records each Ticket it
// reached, so a retry never tells anyone twice, and removes the row.
package eventupdatenotify

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
	// GiveUpAfter is how long after the first change a Notification is
	// tried, and sent at all.
	GiveUpAfter = 24 * time.Hour
	// lease is how long a pass holds the row it took; one row is sent to
	// every holder in it, and renewed every renewEvery sends.
	lease        = 10 * time.Minute
	renewEvery   = 20
	pollInterval = 15 * time.Second
	// maxRows bounds one pass.
	maxRows      = 20
	writeTimeout = 5 * time.Second
	timeZone     = "Europe/Istanbul"
	dateLayout   = "02.01.2006 15:04"
	// keyVersion is the version in the Idempotency-Key: a later change of
	// what one Notification says takes a new one.
	keyVersion = "v1"
)

// Queued is one Event change waiting for its Notification: what the Event
// said before the first change the row gathers.
type Queued struct {
	ID             uuid.UUID
	EventID        uuid.UUID
	BeforeStart    *time.Time
	BeforeEnd      *time.Time
	BeforeLocation string
	// CreatedAt is the first change's time.
	CreatedAt time.Time
	Attempts  int
	// ClaimedAt is the claim that returned the row: Renew, Complete and
	// Retry write only while the row still carries it.
	ClaimedAt time.Time
}

// Queue keeps the Event changes waiting (PostgresQueue:
// event_update_notifications).
type Queue interface {
	// Claim takes the row due first and holds it for lease; false when
	// none is due. A row waits while an older row of the same Event is
	// there, so a later change never overtakes an earlier one.
	Claim(ctx context.Context, now time.Time, lease time.Duration) (Queued, bool, error)
	// Renew holds the claimed row until until, while the pass still holds
	// it.
	Renew(ctx context.Context, claimed Queued, until time.Time) error
	// Sends are the Tickets the row's Notification reached.
	Sends(ctx context.Context, id uuid.UUID) ([]uuid.UUID, error)
	// RecordSend records that the row's Notification reached the Ticket.
	RecordSend(ctx context.Context, id, ticketID uuid.UUID) error
	// Complete removes a claimed row whose Notification is done with.
	Complete(ctx context.Context, claimed Queued) error
	// Retry counts a failure, makes the row due at at and releases it.
	Retry(ctx context.Context, claimed Queued, at time.Time) error
}

// Mailer posts the Notification (mail.SkyMail).
type Mailer interface {
	EventUpdated(ctx context.Context, idempotencyKey string, holder mail.TicketHolder, vars map[string]string, source mail.NotificationSource, expiresAt *time.Time) error
}

// Tickets, Events and Users are what the worker reads when it sends.
type Tickets interface {
	ListByEvent(ctx context.Context, eventID uuid.UUID) ([]ticket.Ticket, error)
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
		return nil, errors.New("eventupdatenotify: a queue, the Tickets, Events and users and a mailer are needed")
	}
	location, err := time.LoadLocation(timeZone)
	if err != nil {
		return nil, fmt.Errorf("eventupdatenotify: %w", err)
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

// Report counts one pass. Changes are the rows done with; Dropped: nothing to
// tell any more (the Event is gone, not published, archived or over, or its
// time and place are back to what they were). Expired: tried for
// GiveUpAfter. Retried: tried again later. The rest are holders: Sent,
// Skipped (no one to write to: an erased or disabled account, a guest
// without an address), Rejected (SkyMail refused the request itself) and
// Uncertain (a guest's mail went out and no answer came; it is not tried
// again, so the guest is not mailed twice).
type Report struct {
	Changes, Dropped, Expired, Retried int
	Sent, Skipped, Rejected, Uncertain int
}

func (r Report) any() bool {
	return r.Changes+r.Dropped+r.Expired+r.Retried+r.Sent+r.Skipped+r.Rejected+r.Uncertain > 0
}

// IdempotencyKey is the key of one holder's Notification of a queued change:
// the type, the row and the person, the same on every try.
func IdempotencyKey(id, sub uuid.UUID) string {
	return "core:" + mail.EventUpdatedType + ":" + id.String() + ":" + sub.String() + ":" + keyVersion
}

// Pass sends the due Notifications until none is due (or maxRows).
func (s *Service) Pass(ctx context.Context) (Report, error) {
	var report Report
	for range maxRows {
		claimed, ok, err := s.config.Queue.Claim(ctx, s.now().UTC(), lease)
		if err != nil || !ok {
			return report, err
		}
		err = s.deliver(ctx, claimed, &report)
		writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), writeTimeout)
		switch {
		case err != nil && ctx.Err() != nil:
			// Core is stopping: the row is taken again once its lease runs
			// out; what it reached is recorded.
			cancel()
			return report, ctx.Err()
		case err != nil:
			report.Retried++
			s.logger.Printf("event updated notification: failed and is tried again: %v", err)
			err = s.config.Queue.Retry(writeCtx, claimed, s.now().UTC().Add(backoff(claimed.Attempts+1)))
		default:
			report.Changes++
			err = s.config.Queue.Complete(writeCtx, claimed)
		}
		cancel()
		if err != nil {
			return report, err
		}
	}
	return report, nil
}

// deliver sends claimed to each holder it has not reached; nil when the row
// is done with (sent, or no longer to be sent), an error when a send failed
// in a way a later try may pass.
func (s *Service) deliver(ctx context.Context, claimed Queued, report *Report) error {
	if s.now().Sub(claimed.CreatedAt) > GiveUpAfter {
		report.Expired++
		return nil
	}
	ev, told, err := s.event(ctx, claimed)
	if err != nil {
		return err
	}
	if !told {
		report.Dropped++
		return nil
	}
	recipients, err := s.recipients(ctx, claimed)
	if err != nil {
		return err
	}
	vars := s.vars(ev, claimed)
	source := mail.NotificationSource{Kind: "event", Ref: ev.ID.String()}
	expires := ev.EndsAt()
	sends := 0
	for _, r := range recipients {
		if r.sent {
			continue
		}
		if !r.ok {
			report.Skipped++
			continue
		}
		// The claim is renewed, and the Event read again, every renewEvery
		// sends: a cancellation while a long Notification goes stops it
		// where it is.
		if sends > 0 && sends%renewEvery == 0 {
			if err := s.config.Queue.Renew(ctx, claimed, s.now().UTC().Add(lease)); err != nil {
				return err
			}
			if _, told, err = s.event(ctx, claimed); err != nil {
				return err
			}
			if !told {
				report.Dropped++
				return nil
			}
		}
		sends++
		err := s.config.Mail.EventUpdated(ctx, IdempotencyKey(claimed.ID, r.holder.Sub), r.holder, vars, source, expires)
		var refused *mail.SendError
		switch {
		case errors.Is(err, mail.ErrMaybeDelivered):
			report.Uncertain++
		case errors.As(err, &refused) && refused.Permanent():
			// SkyMail refuses this body itself (400, 422): the same body
			// would be refused again.
			report.Rejected++
			continue
		case err != nil:
			return err
		default:
			report.Sent++
		}
		for _, id := range r.tickets {
			if err := s.config.Queue.RecordSend(ctx, claimed.ID, id); err != nil {
				return err
			}
		}
	}
	return nil
}

// event reads the Event; told is whether claimed is still news: the Event
// is there, published, not archived and not over, and its time or place is
// not what it was before the first change.
func (s *Service) event(ctx context.Context, claimed Queued) (event.Event, bool, error) {
	ev, err := s.config.Events.GetIncludingArchived(ctx, claimed.EventID)
	if errors.Is(err, event.ErrNotFound) {
		return event.Event{}, false, nil
	}
	if err != nil {
		return event.Event{}, false, err
	}
	if ev.ArchivedAt != nil || ev.Status != event.StatusPublished || ev.EndedAt(s.now()) {
		return ev, false, nil
	}
	timeChanged, placeChanged := changed(ev, claimed)
	return ev, timeChanged || placeChanged, nil
}

// changed says whether the Event's time (start or end) and its place are not
// what they were before the first change.
func changed(ev event.Event, claimed Queued) (timeChanged, placeChanged bool) {
	timeChanged = !sameTime(ev.StartDate, claimed.BeforeStart) || !sameTime(ev.EndDate, claimed.BeforeEnd)
	placeChanged = strings.TrimSpace(ev.Location) != strings.TrimSpace(claimed.BeforeLocation)
	return timeChanged, placeChanged
}

func sameTime(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Equal(*b)
}

// recipient is one holder, with every Ticket of the Event they hold (a
// person may hold a registered and a guest Ticket under their address).
type recipient struct {
	holder  mail.TicketHolder
	ok      bool
	tickets []uuid.UUID
	sent    bool
}

// recipients are the Event's Ticket holders, one per person: a registered
// holder by subject while their account is active, a guest by address. A
// guest Ticket under a registered holder's primary e-mail is theirs. A
// Ticket the Notification reached already counts as sent.
func (s *Service) recipients(ctx context.Context, claimed Queued) ([]*recipient, error) {
	tickets, err := s.config.Tickets.ListByEvent(ctx, claimed.EventID)
	if err != nil {
		return nil, err
	}
	sends, err := s.config.Queue.Sends(ctx, claimed.ID)
	if err != nil {
		return nil, err
	}
	reached := make(map[uuid.UUID]bool, len(sends))
	for _, id := range sends {
		reached[id] = true
	}
	var out []*recipient
	bySub := map[uuid.UUID]*recipient{}
	byEmail := map[string]*recipient{}
	add := func(r *recipient, t ticket.Ticket) {
		r.tickets = append(r.tickets, t.ID)
		r.sent = r.sent || reached[t.ID]
	}
	var guests []ticket.Ticket
	for _, t := range tickets {
		if t.TicketType == ticket.Guest {
			guests = append(guests, t)
			continue
		}
		sub, email, ok, err := s.member(ctx, t)
		if err != nil {
			return nil, err
		}
		if !ok {
			r := &recipient{}
			add(r, t)
			out = append(out, r)
			continue
		}
		r := bySub[sub]
		if r == nil {
			r = &recipient{holder: mail.TicketHolder{Sub: sub}, ok: true}
			bySub[sub] = r
			out = append(out, r)
			if email != "" {
				byEmail[email] = r
			}
		}
		add(r, t)
	}
	for _, t := range guests {
		email := strings.ToLower(strings.TrimSpace(t.GuestEmail))
		r := byEmail[email]
		if r == nil || email == "" {
			r = &recipient{holder: mail.TicketHolder{Email: email, FullName: joinName(t.GuestFirstName, t.GuestLastName)}, ok: email != ""}
			out = append(out, r)
			if email != "" {
				byEmail[email] = r
			}
		}
		add(r, t)
	}
	return out, nil
}

// member is a registered Ticket's holder: their subject and primary e-mail
// while their account is active; not ok when they may not be written to.
func (s *Service) member(ctx context.Context, t ticket.Ticket) (uuid.UUID, string, bool, error) {
	if t.OwnerID == nil || *t.OwnerID == user.DeletedSubject {
		return uuid.Nil, "", false, nil
	}
	u, err := s.config.Users.Get(ctx, *t.OwnerID)
	switch {
	case errors.Is(err, user.ErrNotFound), errors.Is(err, user.ErrAccountBlocked):
		return uuid.Nil, "", false, nil
	case err != nil:
		return uuid.Nil, "", false, err
	case u.AccountState != user.AccountActive:
		return uuid.Nil, "", false, nil
	}
	return *t.OwnerID, strings.ToLower(strings.TrimSpace(u.Email)), true, nil
}

// vars are the type's variables (SkyMail emails/event-updated.tsx), the same
// for every holder: the Event as it is now, and what changed. EventId fills
// the inbox item's link. Every key is always there, a time empty without
// it: SkyMail renders the inbox text with missingkey=error.
func (s *Service) vars(ev event.Event, claimed Queued) map[string]string {
	timeChanged, placeChanged := changed(ev, claimed)
	summary := "Etkinliğin saati ve yeri değişti."
	switch {
	case !placeChanged:
		summary = "Etkinliğin saati değişti."
	case !timeChanged:
		summary = "Etkinliğin yeri değişti."
	}
	return map[string]string{
		"EventId":       ev.ID.String(),
		"EventName":     ev.Name,
		"ChangeSummary": summary,
		"NewStartsAt":   s.format(ev.StartDate),
		"NewEndsAt":     s.format(ev.EndDate),
		"NewVenue":      strings.TrimSpace(ev.Location),
	}
}

func (s *Service) format(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.In(s.location).Format(dateLayout)
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
				logf("event updated notification: queue: %v", err)
			case report.any():
				logf("event updated notification: %d changes done (%d dropped, %d given up), %d tried again later; %d sent, %d skipped, %d refused by SkyMail, %d guest mails unanswered and not tried again",
					report.Changes, report.Dropped, report.Expired, report.Retried, report.Sent, report.Skipped, report.Rejected, report.Uncertain)
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
