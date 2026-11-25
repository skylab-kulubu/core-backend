// Package doorstaffmail tells a person they were added to an Event's door
// staff (docs/door-staff-mail.md): core says what happened and to whom,
// SkyMail picks the channels (inbox and e-mail) and the text. The write that
// adds them queues the mail in its own transaction
// (door_staff_notifications); this worker sends it once the Event is
// published, if they are still on its door staff, and removes the row.
package doorstaffmail

import (
	"context"
	"errors"
	"fmt"
	"log"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/mail"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

const (
	// RetryFirst is the wait before a mail that failed is tried again; it
	// doubles with each failure in a row, up to retryMax.
	RetryFirst = 30 * time.Second
	retryMax   = 30 * time.Minute
	// DraftWait is how long a mail of a draft's door staff waits before
	// the worker looks again whether the Event is published.
	DraftWait = time.Hour
	// GiveUpAfter is how long after the person was added the mail is sent
	// at all: a draft published later than that tells nobody.
	GiveUpAfter  = 30 * 24 * time.Hour
	lease        = 2 * time.Minute
	pollInterval = 15 * time.Second
	maxRows      = 50
	writeTimeout = 5 * time.Second
	timeZone     = "Europe/Istanbul"
	dateLayout   = "02.01.2006 15:04"
	keyVersion   = "v1"
)

// Queued is one waiting mail: an Event and a person added to its door staff.
type Queued struct {
	ID        uuid.UUID
	EventID   uuid.UUID
	UserID    uuid.UUID
	CreatedAt time.Time
	Attempts  int
	// ClaimedAt is the claim that returned the row: Complete and Retry
	// write only while the row still carries it.
	ClaimedAt time.Time
}

// Queue keeps the waiting mails (PostgresQueue: door_staff_notifications).
type Queue interface {
	Claim(ctx context.Context, now time.Time, lease time.Duration) (Queued, bool, error)
	Complete(ctx context.Context, claimed Queued) error
	// Retry makes the row due at at and releases it; failed counts a
	// failure (a draft's wait is none).
	Retry(ctx context.Context, claimed Queued, at time.Time, failed bool) error
}

// Mailer posts the Notification (mail.SkyMail).
type Mailer interface {
	DoorStaffAdded(ctx context.Context, idempotencyKey string, sub uuid.UUID, vars map[string]string, source mail.NotificationSource, expiresAt *time.Time) error
}

type Events interface {
	GetIncludingArchived(ctx context.Context, id uuid.UUID) (event.Event, error)
}

type Users interface {
	Get(ctx context.Context, id uuid.UUID) (user.User, error)
}

// Config is what a Service takes.
type Config struct {
	Queue  Queue
	Events Events
	Users  Users
	Mail   Mailer
	// PanelURL is the panel's origin (PANEL_URL): the mail links the
	// Event's door page there. Empty sends DoorUrl empty, and the template
	// leaves the button out.
	PanelURL string
	// Now is the clock; nil is time.Now.
	Now func() time.Time
	// Logger hears counts and failures, never a person; nil is log.Default.
	Logger *log.Logger
}

// Service sends the queued mails (Run).
type Service struct {
	config   Config
	now      func() time.Time
	logger   *log.Logger
	location *time.Location
}

// NewService checks config.
func NewService(config Config) (*Service, error) {
	if config.Queue == nil || config.Events == nil || config.Users == nil || config.Mail == nil {
		return nil, errors.New("doorstaffmail: a queue, the Events and users and a mailer are needed")
	}
	location, err := time.LoadLocation(timeZone)
	if err != nil {
		return nil, fmt.Errorf("doorstaffmail: %w", err)
	}
	now := config.Now
	if now == nil {
		now = time.Now
	}
	logger := config.Logger
	if logger == nil {
		logger = log.Default()
	}
	config.PanelURL = strings.TrimRight(strings.TrimSpace(config.PanelURL), "/")
	return &Service{config: config, now: now, logger: logger, location: location}, nil
}

// Report counts one pass: Sent; Dropped (no longer to be told: taken off the
// door staff, the Event gone, cancelled, archived or over, the account not
// active); Waiting (a draft's, looked at again later); Expired (older than
// GiveUpAfter); Rejected (SkyMail refused the request itself); Retried.
type Report struct {
	Sent, Dropped, Waiting, Expired, Rejected, Retried int
}

func (r Report) any() bool {
	return r.Sent+r.Dropped+r.Waiting+r.Expired+r.Rejected+r.Retried > 0
}

// IdempotencyKey is the key of one queued mail, the same on every try. A
// person taken off and added again gets a new row and so a new key.
func IdempotencyKey(row Queued) string {
	return "core:" + mail.DoorStaffAddedType + ":" + row.ID.String() + ":" + keyVersion
}

type outcome int

const (
	sent outcome = iota
	dropped
	waiting
	expired
	rejected
)

// Pass sends the due mails until none is due (or maxRows).
func (s *Service) Pass(ctx context.Context) (Report, error) {
	var report Report
	for range maxRows {
		claimed, ok, err := s.config.Queue.Claim(ctx, s.now().UTC(), lease)
		if err != nil || !ok {
			return report, err
		}
		result, err := s.deliver(ctx, claimed)
		writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), writeTimeout)
		switch {
		case err != nil && ctx.Err() != nil:
			cancel()
			return report, ctx.Err()
		case err != nil:
			report.Retried++
			s.logger.Printf("door staff mail: failed and is tried again: %v", err)
			err = s.config.Queue.Retry(writeCtx, claimed, s.now().UTC().Add(backoff(claimed.Attempts+1)), true)
		case result == waiting:
			report.Waiting++
			err = s.config.Queue.Retry(writeCtx, claimed, s.now().UTC().Add(DraftWait), false)
		default:
			switch result {
			case sent:
				report.Sent++
			case dropped:
				report.Dropped++
			case expired:
				report.Expired++
			case rejected:
				report.Rejected++
			}
			err = s.config.Queue.Complete(writeCtx, claimed)
		}
		cancel()
		if err != nil {
			return report, err
		}
	}
	return report, nil
}

// deliver decides and sends one row; an error is a failure a later try may
// pass.
func (s *Service) deliver(ctx context.Context, claimed Queued) (outcome, error) {
	if s.now().Sub(claimed.CreatedAt) > GiveUpAfter {
		return expired, nil
	}
	ev, err := s.config.Events.GetIncludingArchived(ctx, claimed.EventID)
	switch {
	case errors.Is(err, event.ErrNotFound):
		return dropped, nil
	case err != nil:
		return 0, err
	}
	if ev.ArchivedAt != nil || ev.Status == event.StatusCancelled || ev.EndedAt(s.now()) ||
		!slices.Contains(ev.DoorStaffIDs, claimed.UserID) {
		return dropped, nil
	}
	if ev.Status != event.StatusPublished {
		return waiting, nil
	}
	u, err := s.config.Users.Get(ctx, claimed.UserID)
	switch {
	case errors.Is(err, user.ErrNotFound), errors.Is(err, user.ErrAccountBlocked):
		return dropped, nil
	case err != nil:
		return 0, err
	case u.AccountState != user.AccountActive:
		return dropped, nil
	}
	err = s.config.Mail.DoorStaffAdded(ctx, IdempotencyKey(claimed), claimed.UserID, s.vars(ev, u),
		mail.NotificationSource{Kind: "event", Ref: ev.ID.String()}, ev.EndsAt())
	var refused *mail.SendError
	switch {
	case errors.As(err, &refused) && refused.Permanent():
		return rejected, nil
	case err != nil:
		return 0, err
	}
	return sent, nil
}

// vars are the type's variables, every key always there (SkyMail renders the
// inbox text with missingkey=error).
func (s *Service) vars(ev event.Event, u user.User) map[string]string {
	door := ""
	if s.config.PanelURL != "" {
		door = s.config.PanelURL + "/events/" + ev.ID.String() + "/door"
	}
	return map[string]string{
		"FirstName": strings.TrimSpace(u.FirstName),
		"EventId":   ev.ID.String(),
		"EventName": ev.Name,
		"StartsAt":  s.format(ev.StartDate),
		"EndsAt":    s.format(ev.EndDate),
		"Venue":     strings.TrimSpace(ev.Location),
		"DoorUrl":   door,
	}
}

func (s *Service) format(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.In(s.location).Format(dateLayout)
}

func backoff(failures int) time.Duration {
	wait := RetryFirst
	for i := 1; i < failures && wait < retryMax; i++ {
		wait *= 2
	}
	return min(wait, retryMax)
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
				logf("door staff mail: queue: %v", err)
			case report.any():
				logf("door staff mail: %d sent, %d dropped, %d waiting for publication, %d given up, %d refused by SkyMail, %d tried again later",
					report.Sent, report.Dropped, report.Waiting, report.Expired, report.Rejected, report.Retried)
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
