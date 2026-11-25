// Package coorganizermail tells the people of a change of an Event's
// Co-organizers (docs/co-organizers.md#mails): a person added or removed,
// or the Leaders of a team added or removed. Core says what happened and to
// whom, SkyNotify picks the channels (inbox and e-mail) and the text. The
// write of the list queues the mail in its own transaction
// (co_organizer_notifications); this worker sends it once its delay is
// over, if the change still holds, and removes the row.
package coorganizermail

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/mail"
	"github.com/skylab-kulubu/core-backend/internal/organizer"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

const (
	// RetryFirst is the wait before a mail that failed is tried again; it
	// doubles with each failure in a row, up to retryMax.
	RetryFirst = 30 * time.Second
	retryMax   = 30 * time.Minute
	// GiveUpAfter is how long after the change the mail is sent at all.
	GiveUpAfter  = 7 * 24 * time.Hour
	lease        = 2 * time.Minute
	pollInterval = 15 * time.Second
	maxRows      = 50
	writeTimeout = 5 * time.Second
	keyVersion   = "v1"
)

// Queued is one waiting mail: an Event, a team (TeamCode) or a person
// (UserID) added or removed (Kind), and who changed the list.
type Queued struct {
	ID        uuid.UUID
	EventID   uuid.UUID
	Kind      string
	TeamCode  string
	UserID    uuid.UUID
	ActorID   uuid.UUID
	CreatedAt time.Time
	Attempts  int
	// ClaimedAt is the claim that returned the row: Complete and Retry
	// write only while the row still carries it.
	ClaimedAt time.Time
}

// Queue keeps the waiting mails (PostgresQueue: co_organizer_notifications).
type Queue interface {
	Claim(ctx context.Context, now time.Time, lease time.Duration) (Queued, bool, error)
	Complete(ctx context.Context, claimed Queued) error
	Retry(ctx context.Context, claimed Queued, at time.Time) error
}

// Mailer posts the Notification (mail.SkyMail).
type Mailer interface {
	CoOrganizer(ctx context.Context, typ, idempotencyKey string, subs []uuid.UUID, vars map[string]string, source mail.NotificationSource) error
}

// Events reads the Event as it is now.
type Events interface {
	GetIncludingArchived(ctx context.Context, id uuid.UUID) (event.Event, error)
}

// Users reads core's account of a person: the recipient's state, the
// actor's name.
type Users interface {
	Get(ctx context.Context, id uuid.UUID) (user.User, error)
}

// Leaders are the people in a team's Leader groups now (its own, not a Team
// above it), from the directory (DirectoryLeaders).
type Leaders interface {
	LeaderIDs(ctx context.Context, teamCode string) ([]uuid.UUID, error)
}

// Config is what a Service takes.
type Config struct {
	Queue   Queue
	Events  Events
	Users   Users
	Leaders Leaders
	Mail    Mailer
	// PanelURL is the panel's origin (PANEL_URL): the mail links the
	// Event there. Empty sends PanelUrl empty, and the template leaves the
	// button out.
	PanelURL string
	// Now is the clock; nil is time.Now.
	Now func() time.Time
	// Logger hears counts and failures, never a person; nil is log.Default.
	Logger *log.Logger
}

// Service sends the queued mails (Run).
type Service struct {
	config Config
	now    func() time.Time
	logger *log.Logger
}

// NewService checks config.
func NewService(config Config) (*Service, error) {
	if config.Queue == nil || config.Events == nil || config.Users == nil || config.Leaders == nil || config.Mail == nil {
		return nil, errors.New("coorganizermail: a queue, the Events, users, Leaders and a mailer are needed")
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	logger := config.Logger
	if logger == nil {
		logger = log.Default()
	}
	config.PanelURL = strings.TrimRight(strings.TrimSpace(config.PanelURL), "/")
	if err := checkPanelURL(config.PanelURL); err != nil {
		return nil, err
	}
	return &Service{config: config, now: config.Now, logger: logger}, nil
}

// Report counts one pass: Sent; Dropped (the change no longer holds, the
// Event is gone or archived, nobody to tell); Expired (older than
// GiveUpAfter); Rejected (SkyNotify refused the request itself); Retried.
type Report struct {
	Sent, Dropped, Expired, Rejected, Retried int
}

func (r Report) any() bool { return r.Sent+r.Dropped+r.Expired+r.Rejected+r.Retried > 0 }

// Type is the Notification type of a row's kind.
func Type(kind string) string {
	if kind == event.CoOrganizerMailRemoved {
		return mail.CoOrganizerRemovedType
	}
	return mail.CoOrganizerAddedType
}

// IdempotencyKey is the key of one queued mail, the same on every try.
func IdempotencyKey(row Queued) string {
	return "core:" + Type(row.Kind) + ":" + row.ID.String() + ":" + keyVersion
}

type outcome int

const (
	sent outcome = iota
	dropped
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
			s.logger.Printf("co-organizer mail: failed and is tried again: %v", err)
			err = s.config.Queue.Retry(writeCtx, claimed, s.now().UTC().Add(backoff(claimed.Attempts+1)))
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
	if ev.ArchivedAt != nil {
		return dropped, nil
	}
	added := claimed.Kind == event.CoOrganizerMailAdded
	var (
		subs []uuid.UUID
		team event.CoOrganizerTeam
	)
	if claimed.TeamCode != "" {
		i := slices.IndexFunc(ev.CoOrganizers, func(t event.CoOrganizerTeam) bool { return strings.EqualFold(t.Code, claimed.TeamCode) })
		if (i >= 0) != added {
			return dropped, nil
		}
		team = event.CoOrganizerTeam{Code: claimed.TeamCode}
		if i >= 0 {
			team = ev.CoOrganizers[i]
		}
		if subs, err = s.config.Leaders.LeaderIDs(ctx, claimed.TeamCode); err != nil {
			return 0, err
		}
	} else {
		on := slices.Contains(ev.CoOrganizerIDs, claimed.UserID)
		if on != added {
			return dropped, nil
		}
		// A person whose rights have ended is told nothing more.
		if end, bounded := event.CoOrganizerPersonExpiry(ev); added && bounded && !s.now().Before(end) {
			return dropped, nil
		}
		active, err := s.active(ctx, claimed.UserID)
		if err != nil {
			return 0, err
		}
		if active {
			subs = []uuid.UUID{claimed.UserID}
		}
	}
	if len(subs) == 0 {
		return dropped, nil
	}
	err = s.config.Mail.CoOrganizer(ctx, Type(claimed.Kind), IdempotencyKey(claimed), subs, s.vars(ctx, ev, claimed, team),
		mail.NotificationSource{Kind: "event", Ref: ev.ID.String()})
	var refused *mail.SendError
	switch {
	case errors.As(err, &refused) && refused.Permanent():
		return rejected, nil
	case err != nil:
		return 0, err
	}
	return sent, nil
}

// active reports whether the person's account may still be written to.
func (s *Service) active(ctx context.Context, id uuid.UUID) (bool, error) {
	u, err := s.config.Users.Get(ctx, id)
	switch {
	case errors.Is(err, user.ErrNotFound), errors.Is(err, user.ErrAccountBlocked):
		return false, nil
	case err != nil:
		return false, err
	}
	return u.AccountState == user.AccountActive, nil
}

// managementText and visibilityText are a team's choices as the mail
// writes them.
var (
	managementText = map[event.Management]string{event.ManagementLeaders: "yalnız liderler", event.ManagementMembers: "bütün üyeler"}
	visibilityText = map[event.Visibility]string{event.VisibilityCoOrganizer: "ortak düzenleyen", event.VisibilityHelper: "yardımcı ekip (dışarıda görünmez)"}
)

// vars are the type's variables, every key always there (SkyNotify renders
// the inbox text with missingkey=error): a person's mail has no team, a
// removal no choices, an actor who may no longer be named no name.
func (s *Service) vars(ctx context.Context, ev event.Event, claimed Queued, team event.CoOrganizerTeam) map[string]string {
	panel := ""
	if s.config.PanelURL != "" {
		panel = s.config.PanelURL + "/events/" + ev.ID.String()
	}
	teamName := ""
	if team.Code != "" {
		teamName = organizer.Name(team.Code)
	}
	actor := ""
	if claimed.ActorID != uuid.Nil {
		if u, err := s.config.Users.Get(ctx, claimed.ActorID); err == nil && u.AccountState == user.AccountActive {
			actor = strings.TrimSpace(strings.TrimSpace(u.FirstName) + " " + strings.TrimSpace(u.LastName))
		}
	}
	return map[string]string{
		"EventId":    ev.ID.String(),
		"EventName":  ev.Name,
		"OwnerTeam":  organizer.Name(ev.OwnerTeam),
		"TeamName":   teamName,
		"Management": managementText[team.Management],
		"Visibility": visibilityText[team.Visibility],
		"ActorName":  actor,
		"PanelUrl":   panel,
	}
}

// checkPanelURL refuses a PANEL_URL that is not a bare https origin
// (http only for a local panel): the mail's link must not lead elsewhere.
func checkPanelURL(raw string) error {
	if raw == "" {
		return nil
	}
	u, err := url.Parse(raw)
	local := u != nil && (u.Hostname() == "localhost" || u.Hostname() == "127.0.0.1")
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") ||
		(u.Scheme != "https" && !(u.Scheme == "http" && local)) {
		return fmt.Errorf("coorganizermail: PANEL_URL must be the panel's https origin, e.g. https://admin.example")
	}
	return nil
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
				logf("co-organizer mail: queue: %v", err)
			case report.any():
				logf("co-organizer mail: %d sent, %d dropped, %d given up, %d refused by SkyNotify, %d tried again later",
					report.Sent, report.Dropped, report.Expired, report.Rejected, report.Retried)
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
