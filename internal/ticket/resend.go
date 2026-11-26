package ticket

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"path"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/identity"
	"github.com/skylab-kulubu/core-backend/internal/teamaudit"
)

// Sending a Ticket's mail again and renewing its link from the panel
// (ADR 0082; docs/ticket-resend.md).
var (
	// ErrResendLimited is a Ticket sent ResendLimit times in the last
	// ResendWindow; ErrResendDailyLimited ResendDailyLimit times in the
	// last ResendDayWindow.
	ErrResendLimited      = errors.New("ticket: resend limited")
	ErrResendDailyLimited = errors.New("ticket: resend daily limited")
	// ErrNotificationOff is TICKET_CREATED_NOTIFICATION off: no mail goes.
	ErrNotificationOff = errors.New("ticket: ticket notification off")
	// ErrHolderErased is a Ticket whose holder was erased: there is nobody
	// to send it to, nor a link to renew.
	ErrHolderErased = errors.New("ticket: holder erased")
	// ErrEventEnded is an Event that is over: its Tickets are not sent again.
	ErrEventEnded = errors.New("ticket: event ended")
)

const (
	// ResendLimit is how many times a Ticket's mail may go again in
	// ResendWindow, and ResendDailyLimit in ResendDayWindow: a hijacked
	// Leader account cannot flood one person.
	ResendLimit      = 3
	ResendWindow     = time.Hour
	ResendDailyLimit = 6
	ResendDayWindow  = 24 * time.Hour
	// maxRequestKey bounds the client's Idempotency-Key.
	maxRequestKey = 128
)

// Resend is what sending a Ticket's mail again did.
type Resend struct {
	// N is the Ticket's resend count: its Notification's key ends :r<N>.
	N int `json:"resend"`
	// Replayed: a request with the same Idempotency-Key was answered
	// before; nothing new was written.
	Replayed bool `json:"replayed"`
	// Queued: the mail is queued.
	Queued bool `json:"queued"`
}

// ResendLimits bound how often one Ticket is sent again.
type ResendLimits struct {
	PerWindow int
	Window    time.Duration
	PerDay    int
	DayWindow time.Duration
}

// DefaultResendLimits are three an hour and six a day.
var DefaultResendLimits = ResendLimits{PerWindow: ResendLimit, Window: ResendWindow, PerDay: ResendDailyLimit, DayWindow: ResendDayWindow}

// ResendStore keeps the resends (PostgresStore).
type ResendStore interface {
	NotificationsOn() bool
	QueueResend(ctx context.Context, id uuid.UUID, requestHash string, limits ResendLimits) (Resend, error)
	RenewAccessKey(ctx context.Context, id uuid.UUID, notify bool, requestHash string, limits ResendLimits) (Ticket, Resend, error)
}

// RequestHash is what is kept of a client's Idempotency-Key: its SHA-256,
// "" for none; ErrInvalid for one longer than 128 characters.
func RequestHash(key string) (string, error) {
	key = strings.TrimSpace(key)
	if key == "" {
		return "", nil
	}
	if len(key) > maxRequestKey {
		return "", invalid(CodeRequestKeyTooLong)
	}
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:]), nil
}

// managedTicket is the Ticket id names and its Event, for who may give
// out the Event's Tickets: who may assign them (ticket:manage, the owning
// team's Leaders; the rule of Apply-for-other). A draft p may not see is
// ErrNotFound; a Ticket whose holder was erased ErrHolderErased.
func (s *service) managedTicket(ctx context.Context, p authz.Principal, id uuid.UUID) (Ticket, event.Event, error) {
	t, err := s.tickets.Get(ctx, id)
	if err != nil {
		return Ticket{}, event.Event{}, err
	}
	ev, err := s.eventOf(ctx, t.EventID)
	if err != nil {
		return Ticket{}, event.Event{}, err
	}
	if err := s.seen(&p, ev); err != nil {
		return Ticket{}, event.Event{}, err
	}
	if !s.authz.Allow(p, eventResource(ev, authz.TypeTicket), authz.Assign) {
		return Ticket{}, event.Event{}, ErrForbidden
	}
	if !held(t) {
		return Ticket{}, event.Event{}, ErrHolderErased
	}
	// A cancelled (or waitlisted) Ticket is not sent again nor given a new
	// link: 409 ticket_cancelled / ticket_waitlisted, no audit row.
	if refused := inactiveErr(t); refused != nil {
		return Ticket{}, event.Event{}, refused
	}
	return t, ev, nil
}

// mailable is nil when a Ticket of ev may be sent again: a published Event
// that is not over, while the Notifications are on.
func (s *service) mailable(ev event.Event, store ResendStore) error {
	switch {
	case ev.Status == event.StatusCancelled:
		return ErrEventCancelled
	case ev.Status != event.StatusPublished:
		return ErrEventNotPublished
	case ev.EndedAt(time.Now()):
		return ErrEventEnded
	case store == nil || !store.NotificationsOn():
		return ErrNotificationOff
	}
	return nil
}

// ResendTicket queues the Ticket's "Biletin hazır" mail again, under a key
// of its own (…:r<n>).
func (s *service) ResendTicket(ctx context.Context, p authz.Principal, id uuid.UUID, requestKey string) (Resend, error) {
	hash, err := RequestHash(requestKey)
	if err != nil {
		return Resend{}, err
	}
	_, ev, err := s.managedTicket(ctx, p, id)
	if err != nil {
		return Resend{}, err
	}
	store, _ := s.tickets.(ResendStore)
	if err := s.mailable(ev, store); err != nil {
		return Resend{}, err
	}
	record, err := s.beginTicketAudit(ctx, p, ev, teamaudit.ActionTicketResend, id)
	if err != nil {
		return Resend{}, err
	}
	sent, err := store.QueueResend(ctx, id, hash, DefaultResendLimits)
	s.finishTicketAudit(ctx, record, err, sent.N)
	return sent, err
}

// RenewTicketLink gives the Ticket a new access key and the next QR version,
// and with notify queues its mail with the new link. Without notify (the
// panel's notify=false) only the link is renewed, on any Event: an operator
// who ends a leaked link at once.
func (s *service) RenewTicketLink(ctx context.Context, p authz.Principal, id uuid.UUID, notify bool, requestKey string) (Ticket, Resend, error) {
	hash, err := RequestHash(requestKey)
	if err != nil {
		return Ticket{}, Resend{}, err
	}
	_, ev, err := s.managedTicket(ctx, p, id)
	if err != nil {
		return Ticket{}, Resend{}, err
	}
	store, ok := s.tickets.(ResendStore)
	if !ok {
		return Ticket{}, Resend{}, ErrNotificationOff
	}
	if notify {
		if err := s.mailable(ev, store); err != nil {
			return Ticket{}, Resend{}, err
		}
	}
	record, err := s.beginTicketAudit(ctx, p, ev, teamaudit.ActionTicketLinkRenew, id)
	if err != nil {
		return Ticket{}, Resend{}, err
	}
	renewed, sent, err := store.RenewAccessKey(ctx, id, notify, hash, DefaultResendLimits)
	s.finishTicketAudit(ctx, record, err, sent.N)
	if err != nil {
		return Ticket{}, Resend{}, err
	}
	return s.withEvent(ctx, renewed), sent, nil
}

// auditRecord is an open Team audit row of a ticket action: its id and what
// its after says.
type auditRecord struct {
	id    uuid.UUID
	after teamaudit.State
}

// beginTicketAudit opens the Team audit row of a ticket action
// (docs/ticket-resend.md): the actor the request carries (teamaudit.WithActor)
// on their basis, privileged or the Leader of a Team the Event's team sits
// in or under, against the Event's team. Without an audit writer nothing is
// recorded (a test); with one, a row that cannot be opened stops the action
// (teamaudit.ErrUnavailable), as every audited write.
func (s *service) beginTicketAudit(ctx context.Context, p authz.Principal, ev event.Event, action teamaudit.Action, ticketID uuid.UUID) (*auditRecord, error) {
	return s.beginTicketAuditAs(ctx, p, ev, action, ticketID, s.authz.Allow(p, authz.Resource{Type: authz.TypeTicket}, authz.Assign))
}

// beginTicketAuditAs is beginTicketAudit with the basis decided by the
// caller: privileged, else the Leader of a Team the Event's team sits in or
// under.
func (s *service) beginTicketAuditAs(ctx context.Context, p authz.Principal, ev event.Event, action teamaudit.Action, ticketID uuid.UUID, privileged bool) (*auditRecord, error) {
	return s.beginTicketAuditBasis(ctx, p, ev, action, ticketID, privileged, false)
}

// beginTicketAuditBasis is beginTicketAuditAs; with editor, a caller who is
// neither Privileged nor a Leader of the Event's team (or one above it) is
// recorded as an event editor through the Event's team.
func (s *service) beginTicketAuditBasis(ctx context.Context, p authz.Principal, ev event.Event, action teamaudit.Action, ticketID uuid.UUID, privileged, editor bool) (*auditRecord, error) {
	if s.audit == nil {
		return nil, nil
	}
	actor, ok := teamaudit.ActorFrom(ctx)
	if !ok || s.teams == nil {
		return nil, teamaudit.ErrUnavailable
	}
	team, err := s.teams.GetGroup(ctx, ev.OwnerTeam)
	if err != nil {
		return nil, teamaudit.ErrUnavailable
	}
	actor.Basis, actor.ViaGroupID = teamaudit.BasisPrivileged, ""
	if !privileged {
		via, err := s.leaderVia(ctx, p, team)
		if errors.Is(err, ErrForbidden) {
			via, err = s.coLeaderVia(ctx, p, ev)
		}
		switch {
		case err == nil:
			actor.Basis, actor.ViaGroupID = teamaudit.BasisLeader, via
		case (editor || s.coOrganizer(p, ev)) && errors.Is(err, ErrForbidden):
			actor.Basis, actor.ViaGroupID = teamaudit.BasisEventEditor, team.ID
		default:
			return nil, teamaudit.ErrUnavailable
		}
	}
	after := teamaudit.State{EventID: ev.ID.String()}
	// An import (ticket.import) is about the Event, no one Ticket.
	if ticketID != uuid.Nil {
		after.TicketID = ticketID.String()
	}
	id, err := s.audit.Begin(ctx, teamaudit.Record{
		Actor: actor, Action: action, GroupID: team.ID, GroupPath: team.Path, After: &after,
	})
	if err != nil {
		return nil, err
	}
	return &auditRecord{id: id, after: after}, nil
}

// coLeaderVia is the id of the Team whose Leader group lets p act for one
// of ev's Co-organizer teams (docs/co-organizers.md): ErrForbidden when p
// leads none of them.
func (s *service) coLeaderVia(ctx context.Context, p authz.Principal, ev event.Event) (string, error) {
	for _, co := range ev.CoOrganizers {
		team, err := s.teams.GetGroup(ctx, co.Code)
		if err != nil {
			continue
		}
		if via, err := s.leaderVia(ctx, p, team); err == nil {
			return via, nil
		}
	}
	return "", ErrForbidden
}

// coOrganizer reports whether p's right on ev rests on a Co-organizer seat
// (a team's or a person's): such a write is recorded as an event editor's
// through the Event's team.
func (s *service) coOrganizer(p authz.Principal, ev event.Event) bool {
	switch s.authz.EventVia(p, eventResource(ev, authz.TypeEvent)) {
	case authz.ViaCoTeam, authz.ViaCoPerson:
		return true
	}
	return false
}

// leaderVia is the id of the Team whose Leader group lets p act for team:
// the team itself or one it sits under (ADR-0065).
func (s *service) leaderVia(ctx context.Context, p authz.Principal, team identity.Group) (string, error) {
	best := ""
	for _, g := range p.Groups {
		parent, kind := path.Split(strings.TrimRight(g, "/"))
		if kind != "LIDERLER" && kind != "KOORDINATORLER" {
			continue
		}
		parent = strings.TrimRight(parent, "/")
		if parent == "" || (team.Path != parent && !strings.HasPrefix(team.Path, parent+"/")) {
			continue
		}
		// The nearest Team: the longest path.
		if len(parent) > len(best) {
			best = parent
		}
	}
	switch {
	case best == "":
		return "", ErrForbidden
	case best == team.Path:
		return team.ID, nil
	}
	via, err := s.teams.GetGroup(ctx, best)
	if err != nil {
		return "", err
	}
	return via.ID, nil
}

// finishTicketAudit writes the outcome of the row: ok, or failed with a
// code (a refused resend, a lost database).
func (s *service) finishTicketAudit(ctx context.Context, record *auditRecord, err error, resend int) {
	if record == nil {
		return
	}
	outcome, code := teamaudit.OutcomeOK, ""
	if err != nil {
		outcome, code = teamaudit.OutcomeFailed, resendAuditCode(err)
	}
	after := record.after
	after.Resend = resend
	if ferr := s.audit.Finish(ctx, record.id, outcome, code, &after); ferr != nil {
		s.logger.Printf("tickets: audit record %s stays pending", record.id)
	}
}

func resendAuditCode(err error) string {
	switch {
	case errors.Is(err, ErrResendLimited):
		return "rate_limited"
	case errors.Is(err, ErrResendDailyLimited):
		return "daily_limited"
	case errors.Is(err, ErrNotFound):
		return "not_found"
	case errors.Is(err, ErrTicketCheckedIn):
		return "checked_in"
	case errors.Is(err, ErrTicketInCompetition):
		return "in_competition"
	default:
		return "failed"
	}
}
