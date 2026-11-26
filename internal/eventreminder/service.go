// Package eventreminder sends the reminder the day before an Event
// (kontenjan-iptal-hatirlatma 08, SPEC §3.5; docs/event-reminder.md): at
// 18:00 Istanbul time the day before a published Event starts, everyone
// holding an active Ticket of it gets one mail, event.reminder, with their
// Ticket and the way to cancel it. It is a transactional mail about the
// holder's own Ticket: SkyNotify's type is required (no one turns it off).
//
// It follows the feedback mail's schedule (internal/feedbackmail): every
// pass plans what is due under an advisory lock, one row per Ticket in a
// queue that holds ids; the address is read when the mail goes, the rows of
// one address go as one mail, a pass holds a mail under a lease and a
// failure is tried again later. Unlike it, the worker keeps its own pace:
// at most PerMinute mails a minute, so that the evening's reminders leave
// room in the transactional mailbox's bound (30 a minute) for the other
// mails.
package eventreminder

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/mail"
	"github.com/skylab-kulubu/core-backend/internal/organizer"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
	"github.com/skylab-kulubu/core-backend/internal/ticketpage"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

const (
	// PlanHour is the hour, Istanbul time, from which the Events of the
	// next day are planned.
	PlanHour = 18
	// MissedPlanCutoff: an Event that starts sooner than this after now is
	// not planned any more (core was down at 18:00 and the Event starts
	// soon after midnight).
	MissedPlanCutoff = 3 * time.Hour
	// DefaultPerMinute is the worker's pace when EVENT_REMINDER_PER_MINUTE
	// is unset: 20 of the transactional mailbox's 30 a minute.
	DefaultPerMinute = 20
	// RetryFirst is the wait before a mail that failed is tried again; it
	// doubles with each failure in a row, up to retryMax.
	RetryFirst = 30 * time.Second
	retryMax   = 30 * time.Minute
	lease      = 5 * time.Minute
	// pollInterval is a pass's period: often enough that the per-minute
	// bound, not the poll, sets the pace.
	pollInterval = 15 * time.Second
	maxMails     = 50
	writeTimeout = 5 * time.Second
	timeZone     = "Europe/Istanbul"
	dateLayout   = "02.01.2006 15:04"
)

// Window is what a pass plans: the Events that start in [From, To) (the
// next Istanbul day) and after After (MissedPlanCutoff from now).
type Window struct {
	From, To, After time.Time
}

// Mail is one queued row: a Ticket's reminder of the Event starting at
// StartsAt.
type Mail struct {
	ID, EventID, TicketID uuid.UUID
	StartsAt              time.Time
	CreatedAt             time.Time
	NextAttemptAt         time.Time
	Attempts              int
	// ClaimedAt is the claim that returned the row: Finish and Retry write
	// only while the row still carries it.
	ClaimedAt time.Time
}

// Outcome is how a row was done with.
type Outcome string

const (
	Sent      Outcome = "sent"
	Dropped   Outcome = "dropped"
	Skipped   Outcome = "skipped"
	Rejected  Outcome = "rejected"
	Expired   Outcome = "expired"
	Uncertain Outcome = "uncertain"
)

// Queue is the reminder's plan and queue (PostgresQueue).
type Queue interface {
	Plan(ctx context.Context, now time.Time, w Window) (events, mails int, err error)
	Claim(ctx context.Context, now time.Time, lease time.Duration, perMinute int) (m Mail, ok, limited bool, err error)
	Siblings(ctx context.Context, claimed Mail, email string, now time.Time) ([]Mail, error)
	ClaimSibling(ctx context.Context, id uuid.UUID, now time.Time, lease time.Duration) (Mail, bool, error)
	Finish(ctx context.Context, claimed Mail, at time.Time, outcome Outcome) error
	Retry(ctx context.Context, claimed Mail, at time.Time) error
}

// Mailer posts the reminder (mail.SkyNotify).
type Mailer interface {
	EventReminder(ctx context.Context, typ, idempotencyKey string, holder mail.TicketHolder, vars map[string]any, source mail.NotificationSource, expiresAt *time.Time) error
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
	// Type is the template key (SKYNOTIFY_EVENT_REMINDER_TEMPLATE_KEY):
	// empty plans and sends nothing.
	Type string
	// Page is the ticket page's settings (docs/ticket-page.md): while its
	// URL is set, the mail carries the Ticket's page, QR and code.
	Page ticket.PageConfig
	// APIOrigin is the public API's origin the QR image is read from
	// (PUBLIC_API_ORIGIN); empty leaves TicketQrUrl empty.
	APIOrigin string
	// PerMinute bounds the mails a minute; 0 is no bound.
	PerMinute int
	// Now is the clock; nil is time.Now.
	Now func() time.Time
	// Logger hears counts and failures, never a person; nil is log.Default.
	Logger *log.Logger
}

// Service plans and sends the reminders (Run).
type Service struct {
	config   Config
	now      func() time.Time
	logger   *log.Logger
	location *time.Location
}

// NewService checks config.
func NewService(config Config) (*Service, error) {
	if config.Queue == nil || config.Tickets == nil || config.Events == nil || config.Users == nil || config.Mail == nil {
		return nil, errors.New("eventreminder: a queue, the Tickets, Events and users and a mailer are needed")
	}
	if config.PerMinute < 0 {
		return nil, errors.New("eventreminder: the per-minute bound is 0 (none) or more")
	}
	location, err := time.LoadLocation(timeZone)
	if err != nil {
		return nil, fmt.Errorf("eventreminder: %w", err)
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

// PlanWindow is what a pass at now plans: from PlanHour, the Events that
// start the next Istanbul day and more than MissedPlanCutoff after now;
// before it, nothing (false). Istanbul keeps UTC+3 all year, but the day is
// read from the zone all the same.
func PlanWindow(now time.Time, location *time.Location) (Window, bool) {
	local := now.In(location)
	if local.Hour() < PlanHour {
		return Window{}, false
	}
	y, m, d := local.Date()
	return Window{
		From:  time.Date(y, m, d+1, 0, 0, 0, 0, location),
		To:    time.Date(y, m, d+2, 0, 0, 0, 0, location),
		After: now.Add(MissedPlanCutoff),
	}, true
}

// Report counts one pass: Events and Planned rows; mails Sent, Dropped
// (the Event is no longer published, or not on that day), Skipped (no one
// to write to), Rejected by SkyNotify, Expired (the Event's day began before
// it went), Uncertain (a guest's mail went out and no answer came; it is
// not tried again) and Retried later. Limited is true when the per-minute
// bound stopped the pass.
type Report struct {
	Events, Planned                                      int
	Sent, Dropped, Skipped, Rejected, Expired, Uncertain int
	Retried                                              int
	Limited                                              bool
}

func (r Report) any() bool {
	return r.Events+r.Planned+r.Sent+r.Dropped+r.Skipped+r.Rejected+r.Expired+r.Uncertain+r.Retried > 0
}

// IdempotencyKey is a member's reminder's key: the type, the Event, its
// start and the Ticket the mail carries; the same on every try.
func IdempotencyKey(eventID uuid.UUID, startsAt time.Time, ticketID uuid.UUID) string {
	return "core:" + mail.EventReminderType + ":" + eventID.String() + ":" + strconv.FormatInt(startsAt.Unix(), 10) + ":" + ticketID.String()
}

// Pass plans what is due, then sends the due mails until none is due, the
// per-minute bound is reached, or maxMails.
func (s *Service) Pass(ctx context.Context) (Report, error) {
	var report Report
	if strings.TrimSpace(s.config.Type) == "" {
		return report, nil
	}
	now := s.now().UTC()
	if w, ok := PlanWindow(now, s.location); ok {
		events, planned, err := s.config.Queue.Plan(ctx, now, w)
		if err != nil {
			return report, fmt.Errorf("plan: %w", err)
		}
		report.Events, report.Planned = events, planned
	}
	for range maxMails {
		claimed, ok, limited, err := s.config.Queue.Claim(ctx, s.now().UTC(), lease, s.config.PerMinute)
		if limited {
			report.Limited = true
		}
		if err != nil || !ok {
			return report, err
		}
		group, outcome, err := s.deliver(ctx, claimed, &report)
		writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), writeTimeout)
		switch {
		case err != nil && ctx.Err() != nil:
			cancel()
			return report, ctx.Err()
		case err != nil:
			report.Retried++
			s.logger.Printf("event reminder: a mail failed and is tried again: %v", err)
			at := s.now().UTC().Add(backoff(claimed.Attempts + 1))
			for _, m := range group {
				if err = s.config.Queue.Retry(writeCtx, m, at); err != nil {
					break
				}
			}
		default:
			for _, m := range group {
				if err = s.config.Queue.Finish(writeCtx, m, s.now().UTC(), outcome); err != nil {
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

// recipient is a Ticket's holder: a member's subject (uuid.Nil for a
// guest), the address the rows are gathered by and the guest's name.
type recipient struct {
	ticket ticket.Ticket
	sub    uuid.UUID
	email  string
	full   string
}

// deliver sends the claimed row's mail, with the open rows of the same
// reminder held under the same address (a person with a member and a guest
// Ticket gets one mail, carrying the member's own Ticket). It answers the
// rows it took and how they are done with; an error when a later try may
// pass.
func (s *Service) deliver(ctx context.Context, claimed Mail, report *Report) ([]Mail, Outcome, error) {
	group := []Mail{claimed}
	ev, err := s.config.Events.GetIncludingArchived(ctx, claimed.EventID)
	if errors.Is(err, event.ErrNotFound) {
		report.Dropped++
		return group, Dropped, nil
	}
	if err != nil {
		return group, "", err
	}
	// The Event's day began: "Yarın" is not true any more, and a reminder
	// that missed its evening is not sent late.
	if !s.now().Before(s.dayStart(claimed.StartsAt)) {
		report.Expired++
		return group, Expired, nil
	}
	// A cancelled, unpublished or archived Event, or one moved to another
	// day (that day's reminder is planned of its own), is not reminded.
	if ev.ArchivedAt != nil || ev.Status != event.StatusPublished || ev.StartDate == nil ||
		!s.dayStart(*ev.StartDate).Equal(s.dayStart(claimed.StartsAt)) || !ev.StartDate.After(s.now()) {
		report.Dropped++
		return group, Dropped, nil
	}
	to, ok, err := s.recipientOf(ctx, claimed)
	if err != nil {
		return group, "", err
	}
	if !ok {
		report.Skipped++
		return group, Skipped, nil
	}
	siblings, err := s.config.Queue.Siblings(ctx, claimed, to.email, s.now().UTC())
	if err != nil {
		return group, "", err
	}
	for _, m := range siblings {
		r, ok, err := s.recipientOf(ctx, m)
		if err != nil {
			return group, "", err
		}
		if !ok || r.email != to.email {
			continue
		}
		taken, ok, err := s.config.Queue.ClaimSibling(ctx, m.ID, s.now().UTC(), lease)
		if err != nil {
			return group, "", err
		}
		if !ok {
			continue
		}
		group = append(group, taken)
		// The address is a member's: the mail is theirs, inbox too, and
		// carries their own Ticket.
		if to.sub == uuid.Nil && r.sub != uuid.Nil {
			to = r
		}
	}
	holder := mail.TicketHolder{Sub: to.sub}
	if to.sub == uuid.Nil {
		holder = mail.TicketHolder{Email: to.email, FullName: to.full}
	}
	source := mail.NotificationSource{Kind: "event", Ref: ev.ID.String()}
	key := IdempotencyKey(ev.ID, claimed.StartsAt, to.ticket.ID)
	err = s.config.Mail.EventReminder(ctx, s.config.Type, key, holder, s.vars(ev, to.ticket), source, ev.EndsAt())
	var refused *mail.SendError
	switch {
	case errors.Is(err, mail.ErrMaybeDelivered):
		report.Uncertain++
		return group, Uncertain, nil
	case errors.As(err, &refused) && refused.Permanent():
		// SkyNotify refuses this body itself (400, 422): the same body would
		// be refused again.
		report.Rejected++
		return group, Rejected, nil
	case err != nil:
		return group, "", err
	}
	report.Sent++
	return group, Sent, nil
}

// recipientOf is the row's Ticket's holder; false when there is nobody to
// write to: no Ticket, one no longer active (cancelled or waitlisted since
// it was planned), an erased or disabled account, no address.
func (s *Service) recipientOf(ctx context.Context, m Mail) (recipient, bool, error) {
	t, err := s.config.Tickets.Get(ctx, m.TicketID)
	if errors.Is(err, ticket.ErrNotFound) {
		return recipient{}, false, nil
	}
	if err != nil {
		return recipient{}, false, err
	}
	if !t.Active() || t.EventID != m.EventID {
		return recipient{}, false, nil
	}
	if t.TicketType == ticket.Guest {
		email := strings.ToLower(strings.TrimSpace(t.GuestEmail))
		return recipient{ticket: t, email: email, full: joinName(t.GuestFirstName, t.GuestLastName)}, email != "", nil
	}
	if t.OwnerID == nil || *t.OwnerID == user.DeletedSubject {
		return recipient{}, false, nil
	}
	u, err := s.config.Users.Get(ctx, *t.OwnerID)
	switch {
	case errors.Is(err, user.ErrNotFound), errors.Is(err, user.ErrAccountBlocked):
		return recipient{}, false, nil
	case err != nil:
		return recipient{}, false, err
	case u.AccountState != user.AccountActive:
		return recipient{}, false, nil
	}
	return recipient{ticket: t, sub: *t.OwnerID, email: strings.ToLower(strings.TrimSpace(u.Email))}, true, nil
}

// vars are the type's variables (skynotify-frontend emails/event-reminder.tsx,
// #179): the Event as it is now and the Ticket the mail carries. Every key
// is always there, empty without a value: SkyNotify renders with
// missingkey=error. TicketUrl, TicketQrUrl and TicketCode while
// TICKET_PAGE_URL is set (the ticket created mail's rule); MapUrl is the
// ticket page's directions link.
// TicketNote and TicketNoteParts are the organizer's announcement
// (docs/ticket-note.md), "" and [] for none.
func (s *Service) vars(ev event.Event, t ticket.Ticket) map[string]any {
	vars := map[string]any{
		"EventId":   ev.ID.String(),
		"EventName": ev.Name,
		"StartsAt":  "",
		"Venue":     strings.TrimSpace(ev.Location),
		"OwnerTeam": organizer.Name(ev.OwnerTeam),
		// The Co-organizer teams shown outside the panel (docs/co-organizers.md).
		"CoOrganizers": organizer.CoNames(event.PublicCoOrganizerTeams(ev)),
		"TicketUrl":    "",
		"TicketQrUrl":  "",
		"TicketCode":   "",
		"MapUrl":       ticketpage.DirectionsURL(ev.Location, ev.LocationAddress, ev.LocationLat, ev.LocationLng),
	}
	if ev.StartDate != nil {
		vars["StartsAt"] = ev.StartDate.In(s.location).Format(dateLayout)
	}
	if link := s.config.Page.Link(t.AccessKey); link != "" {
		vars["TicketUrl"] = link
		vars["TicketCode"] = t.Code
		if origin := strings.TrimRight(s.config.APIOrigin, "/"); origin != "" {
			vars["TicketQrUrl"] = origin + "/v1/public/tickets/" + t.AccessKey + "/qr.png"
		}
	}
	vars["TicketNote"], vars["TicketNoteParts"] = event.TicketNoteParts(ev)
	return vars
}

// dayStart is the start of t's Istanbul day.
func (s *Service) dayStart(t time.Time) time.Time {
	y, m, d := t.In(s.location).Date()
	return time.Date(y, m, d, 0, 0, 0, 0, s.location)
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

// Run plans and sends until ctx is cancelled; the channel closes once it
// has stopped.
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
				logf("event reminder: %v", err)
			case report.any():
				logf("event reminder: %d events and %d tickets planned; %d sent, %d dropped, %d skipped, %d refused by SkyNotify, %d expired, %d guest mails unanswered and not tried again, %d tried again later",
					report.Events, report.Planned, report.Sent, report.Dropped, report.Skipped, report.Rejected, report.Expired, report.Uncertain, report.Retried)
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
