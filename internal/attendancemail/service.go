// Package attendancemail sends the door's mails (kapi-postalari 04, 05;
// docs/attendance-mail.md): "hoş geldin" when a Ticket's holder first enters
// the venue on a day, and "katılımın alındı" with their certificate count
// on each Session Check-in, however it was written (the door's scan, the
// attendance QR, the person's own, the desk). They are transactional mails
// of the holder's own scan: SkyMail's types are required (the person cannot
// turn them off); the organizer can, per Event.
//
// The ticket store queues a row in the statement that writes the Venue
// entry or the Check-in (attendance_mails), due AttendanceMailDelay later:
// one welcome a Ticket a day, one receipt a Ticket a Session, so a second
// scan tells nobody again. The worker follows the reminder's queue
// (internal/eventreminder): a pass claims the row due first under a lease
// and at most PerMinute mails a minute; a welcome takes the same Ticket's
// waiting receipts of that day into one mail, and a receipt the other due
// receipts. Just before it sends it reads again whether the Venue entry or
// the Check-in is still there (an undo sends nothing), and counts the
// certificate rule as it stands then.
package attendancemail

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/mail"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// The kinds of a queued row (attendance_mails.kind).
const (
	KindWelcome  = "welcome"
	KindRecorded = "recorded"
)

const (
	// DefaultPerMinute is the worker's pace when ATTENDANCE_MAIL_PER_MINUTE
	// is unset: 20 of the transactional mailbox's 30 a minute, so a
	// Session's start leaves room for the other mails (SPEC §6).
	DefaultPerMinute = 20
	// GiveUpAfter is how long a row is tried: a receipt a day late is no
	// longer news.
	GiveUpAfter = 24 * time.Hour
	// Keep is how long a done row stays: it keeps a second scan from
	// telling anyone again, and is purged after.
	Keep = 30 * 24 * time.Hour
	// RetryFirst is the wait before a mail that failed is tried again; it
	// doubles with each failure in a row, up to retryMax.
	RetryFirst   = 30 * time.Second
	retryMax     = 30 * time.Minute
	lease        = 5 * time.Minute
	pollInterval = 15 * time.Second
	maxMails     = 50
	writeTimeout = 5 * time.Second
	timeZone     = "Europe/Istanbul"
	// keyVersion is the version in the Idempotency-Key: a later change of
	// what a mail says takes a new one.
	keyVersion = "v1"
)

// Mail is one queued row: a Ticket's welcome on EntryDate, or its receipt
// of the Check-in on SessionID.
type Mail struct {
	ID, TicketID, EventID uuid.UUID
	Kind                  string
	// EntryDate is the Istanbul day, YYYY-MM-DD.
	EntryDate     string
	SessionID     *uuid.UUID
	CreatedAt     time.Time
	NextAttemptAt time.Time
	Attempts      int
	// ClaimedAt is the claim that returned the row: Finish and Retry write
	// only while the row still carries it.
	ClaimedAt time.Time
	// MailKey is the Idempotency-Key of the first try that carried the row,
	// empty before any: a retry keeps the rows and the key of that try.
	MailKey string
}

// Outcome is how a row was done with.
type Outcome string

const (
	Sent Outcome = "sent"
	// Dropped: nothing to tell any more (the entry or the Check-in was
	// undone, the Ticket cancelled, the Event unpublished, the organizer's
	// switch turned off). A new scan queues the row again.
	Dropped   Outcome = "dropped"
	Skipped   Outcome = "skipped"
	Rejected  Outcome = "rejected"
	Expired   Outcome = "expired"
	Uncertain Outcome = "uncertain"
)

// Session is a live Session of the Event as the mail reads it.
type Session struct {
	ID       uuid.UUID
	Title    string
	DayID    uuid.UUID
	Start    *time.Time
	End      *time.Time
	Ends     *time.Time
	Counted  bool
	Required bool
}

// Day is a current day of the Event, in the programme's order.
type Day struct {
	ID    uuid.UUID
	Start *time.Time
}

// Progress is the Event's programme and a Ticket's Check-ins, read when a
// mail goes.
type Progress struct {
	// Sessions are the live Sessions (current day, not archived, not
	// cancelled), in the programme's order.
	Sessions []Session
	Days     []Day
	// Attended are the Sessions the Ticket has a Check-in on.
	Attended map[uuid.UUID]bool
	// Titles name the Sessions the mail's rows are about, live or not.
	Titles map[uuid.UUID]string
}

// Queue is the door's mails' queue (PostgresQueue).
type Queue interface {
	// Claim takes the row due first, welcomes before receipts, and holds it
	// for lease, unless perMinute mails (perMinute > 0) went in the minute
	// before now: then limited is true and nothing is taken.
	Claim(ctx context.Context, now time.Time, lease time.Duration, perMinute int) (m Mail, ok, limited bool, err error)
	// Siblings are the open rows that go with the claimed one, not held by
	// another pass: a welcome's are the same Ticket's receipts of that day,
	// due or not; a receipt's the same Ticket's receipts of that day that
	// are due. A row tried before (MailKey) takes only the rows of its try,
	// a row never tried only rows never tried.
	Siblings(ctx context.Context, claimed Mail, now time.Time) ([]Mail, error)
	// Keyed writes key on the rows that have none, before their first try.
	Keyed(ctx context.Context, rows []Mail, key string) error
	// Attempted counts m's mail in the minute at at: the row that led the
	// mail was dropped from it, so the minute's count needs another.
	Attempted(ctx context.Context, m Mail, at time.Time) error
	ClaimSibling(ctx context.Context, id uuid.UUID, now time.Time, lease time.Duration) (Mail, bool, error)
	// Present reports whether the row's Venue entry or Check-in is still
	// there; a receipt's Session must still be live (not cancelled, not
	// archived, on a current day).
	Present(ctx context.Context, m Mail) (bool, error)
	// Progress reads the Event's programme and the Ticket's Check-ins, and
	// the titles of sessions.
	Progress(ctx context.Context, eventID, ticketID uuid.UUID, sessions []uuid.UUID) (Progress, error)
	Finish(ctx context.Context, claimed Mail, at time.Time, outcome Outcome) error
	Retry(ctx context.Context, claimed Mail, at time.Time) error
	// Purge removes the rows done before before.
	Purge(ctx context.Context, before time.Time) (int, error)
}

// Mailer posts the mail (mail.SkyMail).
type Mailer interface {
	AttendanceMail(ctx context.Context, typ, idempotencyKey string, holder mail.TicketHolder, vars map[string]any, source mail.NotificationSource, expiresAt *time.Time) error
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
	// WelcomeType and RecordedType are the template keys
	// (SKYMAIL_ATTENDANCE_WELCOME_TEMPLATE_KEY,
	// SKYMAIL_ATTENDANCE_RECORDED_TEMPLATE_KEY): empty drops that kind's
	// rows (the store queues none then).
	WelcomeType, RecordedType string
	// PerMinute bounds the mails a minute; 0 is no bound.
	PerMinute int
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
	if config.Queue == nil || config.Tickets == nil || config.Events == nil || config.Users == nil || config.Mail == nil {
		return nil, errors.New("attendancemail: a queue, the Tickets, Events and users and a mailer are needed")
	}
	if config.PerMinute < 0 {
		return nil, errors.New("attendancemail: the per-minute bound is 0 (none) or more")
	}
	location, err := time.LoadLocation(timeZone)
	if err != nil {
		return nil, fmt.Errorf("attendancemail: %w", err)
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

// Report counts one pass: mails Sent (Merged the rows that went along in
// another's mail), rows Dropped, Skipped (no one to write to), Rejected by
// SkyMail, Expired (tried for GiveUpAfter), Uncertain (a guest's mail went
// out and no answer came; not tried again) and Retried later; Purged done
// rows. Limited is true when the per-minute bound stopped the pass.
type Report struct {
	Sent, Merged, Dropped, Skipped, Rejected, Expired, Uncertain, Retried, Purged int
	Limited                                                                       bool
}

func (r Report) any() bool {
	return r.Sent+r.Merged+r.Dropped+r.Skipped+r.Rejected+r.Expired+r.Uncertain+r.Retried+r.Purged > 0
}

// IdempotencyKey is a mail's key: the kind and its rows' Ticket and day or
// Session, the same on every try of the same rows (and for the same rows
// queued again after an undo).
func IdempotencyKey(kind string, rows []Mail) string {
	parts := make([]string, 0, len(rows))
	for _, m := range rows {
		part := m.Kind + ":" + m.TicketID.String() + ":" + m.EntryDate
		if m.SessionID != nil {
			part += ":" + m.SessionID.String()
		}
		parts = append(parts, part)
	}
	sort.Strings(parts)
	sum := sha256.Sum256([]byte(strings.Join(parts, "|")))
	return "core:attendance-mail:" + kind + ":" + hex.EncodeToString(sum[:16]) + ":" + keyVersion
}

// Pass purges the old done rows, then sends the due mails until none is
// due, the per-minute bound is reached, or maxMails.
func (s *Service) Pass(ctx context.Context) (Report, error) {
	var report Report
	purged, err := s.config.Queue.Purge(ctx, s.now().UTC().Add(-Keep))
	if err != nil {
		return report, fmt.Errorf("purge: %w", err)
	}
	report.Purged = purged
	for range maxMails {
		claimed, ok, limited, err := s.config.Queue.Claim(ctx, s.now().UTC(), lease, s.config.PerMinute)
		if limited {
			report.Limited = true
		}
		if err != nil || !ok {
			return report, err
		}
		send, drop, outcome, err := s.deliver(ctx, claimed, &report)
		writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), writeTimeout)
		at := s.now().UTC()
		for _, m := range drop {
			if werr := s.config.Queue.Finish(writeCtx, m, at, Dropped); werr != nil && err == nil {
				err = werr
			}
		}
		switch {
		case err != nil && ctx.Err() != nil:
			cancel()
			return report, ctx.Err()
		case err != nil:
			report.Retried++
			s.logger.Printf("attendance mail: a mail failed and is tried again: %v", err)
			retryAt := at.Add(backoff(claimed.Attempts + 1))
			for _, m := range send {
				if err = s.config.Queue.Retry(writeCtx, m, retryAt); err != nil {
					break
				}
			}
		default:
			for _, m := range send {
				if err = s.config.Queue.Finish(writeCtx, m, at, outcome); err != nil {
					break
				}
			}
			// The claimed row led the minute's count, but was dropped from
			// the mail that went: the count moves to a row of that mail.
			if err == nil && len(send) > 0 && countsInMinute(outcome) && contains(drop, claimed.ID) {
				err = s.config.Queue.Attempted(writeCtx, send[0], claimed.ClaimedAt)
			}
		}
		cancel()
		if err != nil {
			return report, err
		}
	}
	return report, nil
}

// deliver sends the claimed row's mail, with the rows that go along. It
// answers the rows the mail was about (send, done with outcome or tried
// again on an error) and the rows that are no longer news (drop); an
// error when a later try may pass.
func (s *Service) deliver(ctx context.Context, claimed Mail, report *Report) (send, drop []Mail, outcome Outcome, err error) {
	group := []Mail{claimed}
	dropAll := func() ([]Mail, []Mail, Outcome, error) {
		report.Dropped += len(group)
		return nil, group, Dropped, nil
	}
	if s.now().Sub(claimed.CreatedAt) > GiveUpAfter {
		report.Expired++
		return group, nil, Expired, nil
	}
	ev, err := s.config.Events.GetIncludingArchived(ctx, claimed.EventID)
	if errors.Is(err, event.ErrNotFound) {
		return dropAll()
	}
	if err != nil {
		return group, nil, "", err
	}
	// Only a published, current Event's door tells anyone: a cancelled or
	// archived one's scans are not news.
	if ev.ArchivedAt != nil || ev.Status != event.StatusPublished {
		return dropAll()
	}
	t, err := s.config.Tickets.Get(ctx, claimed.TicketID)
	if errors.Is(err, ticket.ErrNotFound) {
		return dropAll()
	}
	if err != nil {
		return group, nil, "", err
	}
	if !t.Active() || t.EventID != ev.ID {
		return dropAll()
	}
	siblings, err := s.config.Queue.Siblings(ctx, claimed, s.now().UTC())
	if err != nil {
		return group, nil, "", err
	}
	for _, m := range siblings {
		taken, ok, err := s.config.Queue.ClaimSibling(ctx, m.ID, s.now().UTC(), lease)
		if err != nil {
			return group, nil, "", err
		}
		if ok {
			group = append(group, taken)
		}
	}
	// Each row still news: its kind on, the Event's switch on, its entry or
	// Check-in still there (an undo within the delay sends nothing).
	for _, m := range group {
		keep := false
		switch m.Kind {
		case KindWelcome:
			keep = s.config.WelcomeType != "" && on(ev.AttendanceWelcomeMail)
		case KindRecorded:
			keep = s.config.RecordedType != "" && on(ev.AttendanceRecordedMail)
		}
		if keep {
			present, err := s.config.Queue.Present(ctx, m)
			if err != nil {
				return group, nil, "", err
			}
			keep = present
		}
		if keep {
			send = append(send, m)
		} else {
			drop = append(drop, m)
		}
	}
	report.Dropped += len(drop)
	if len(send) == 0 {
		return nil, drop, Dropped, nil
	}
	to, firstName, ok, err := s.recipientOf(ctx, t)
	if err != nil {
		return send, drop, "", err
	}
	if !ok {
		report.Skipped += len(send)
		return send, drop, Skipped, nil
	}
	kind, typ := KindRecorded, s.config.RecordedType
	var sessionIDs []uuid.UUID
	for _, m := range send {
		if m.Kind == KindWelcome {
			kind, typ = KindWelcome, s.config.WelcomeType
		}
		if m.SessionID != nil {
			sessionIDs = append(sessionIDs, *m.SessionID)
		}
	}
	progress, err := s.config.Queue.Progress(ctx, ev.ID, t.ID, sessionIDs)
	if err != nil {
		return send, drop, "", err
	}
	vars := s.vars(kind, ev, firstName, send, progress)
	source := mail.NotificationSource{Kind: "event", Ref: ev.ID.String()}
	// A retry sends under the key of the first try, whatever rows dropped
	// out since: SkyMail may have taken that try with no answer, and then
	// answers this one from it rather than mailing again. Rows queued since
	// are not in the group (Siblings), so they go in a mail of their own.
	key := claimed.MailKey
	if key == "" {
		key = IdempotencyKey(kind, send)
		if err := s.config.Queue.Keyed(ctx, send, key); err != nil {
			return send, drop, "", err
		}
	}
	err = s.config.Mail.AttendanceMail(ctx, typ, key, to, vars, source, ev.EndsAt())
	var refused *mail.SendError
	switch {
	case errors.Is(err, mail.ErrMaybeDelivered):
		report.Uncertain++
		return send, drop, Uncertain, nil
	case errors.As(err, &refused) && refused.KeyReused():
		// A retry under the first try's key with fewer rows: SkyMail took the
		// first try, whose answer was lost. The mail went.
		report.Sent++
		return send, drop, Sent, nil
	case errors.As(err, &refused) && refused.Permanent():
		// SkyMail refuses this body itself (400, 422): the same body would
		// be refused again.
		report.Rejected++
		return send, drop, Rejected, nil
	case err != nil:
		return send, drop, "", err
	}
	report.Sent++
	report.Merged += len(send) - 1
	return send, drop, Sent, nil
}

// countsInMinute reports whether a row done with outcome made a request to
// SkyMail: those count in the per-minute bound.
func countsInMinute(outcome Outcome) bool {
	return outcome == Sent || outcome == Rejected || outcome == Uncertain
}

func contains(rows []Mail, id uuid.UUID) bool {
	for _, m := range rows {
		if m.ID == id {
			return true
		}
	}
	return false
}

// on is a switch that is on unless it says off.
func on(v *bool) bool { return v == nil || *v }

// recipientOf is the Ticket's holder and their first name; false when there
// is nobody to write to: an erased or disabled account, no address.
func (s *Service) recipientOf(ctx context.Context, t ticket.Ticket) (mail.TicketHolder, string, bool, error) {
	if t.TicketType == ticket.Guest {
		email := strings.ToLower(strings.TrimSpace(t.GuestEmail))
		if email == "" {
			return mail.TicketHolder{}, "", false, nil
		}
		full := strings.TrimSpace(strings.TrimSpace(t.GuestFirstName) + " " + strings.TrimSpace(t.GuestLastName))
		return mail.TicketHolder{Email: email, FullName: full}, strings.TrimSpace(t.GuestFirstName), true, nil
	}
	if t.OwnerID == nil || *t.OwnerID == user.DeletedSubject {
		return mail.TicketHolder{}, "", false, nil
	}
	u, err := s.config.Users.Get(ctx, *t.OwnerID)
	switch {
	case errors.Is(err, user.ErrNotFound), errors.Is(err, user.ErrAccountBlocked):
		return mail.TicketHolder{}, "", false, nil
	case err != nil:
		return mail.TicketHolder{}, "", false, err
	case u.AccountState != user.AccountActive:
		return mail.TicketHolder{}, "", false, nil
	}
	return mail.TicketHolder{Sub: *t.OwnerID}, strings.TrimSpace(u.FirstName), true, nil
}

func backoff(failures int) time.Duration {
	wait := RetryFirst
	for i := 1; i < failures && wait < retryMax; i++ {
		wait *= 2
	}
	return min(wait, retryMax)
}

// Run sends until ctx is cancelled; the channel closes once it has
// stopped.
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
				logf("attendance mail: %v", err)
			case report.any():
				logf("attendance mail: %d sent (%d rows merged into them), %d dropped, %d skipped, %d refused by SkyMail, %d expired, %d guest mails unanswered and not tried again, %d tried again later, %d old rows purged",
					report.Sent, report.Merged, report.Dropped, report.Skipped, report.Rejected, report.Expired, report.Uncertain, report.Retried, report.Purged)
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
