package ticket

import (
	"context"
	"errors"
	"strings"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

type Service interface {
	Apply(ctx context.Context, p authz.Principal, eventID uuid.UUID) (Ticket, error)
	ApplyGuest(ctx context.Context, eventID uuid.UUID, g GuestInfo) (Ticket, error)
	Mine(ctx context.Context, p authz.Principal) ([]Ticket, error)
	ListByEvent(ctx context.Context, p authz.Principal, eventID uuid.UUID) ([]Ticket, error)
	Get(ctx context.Context, p authz.Principal, id uuid.UUID) (Ticket, error)
	GetByUserEvent(ctx context.Context, p authz.Principal, userID, eventID uuid.UUID) (Ticket, error)
	ListQuery(ctx context.Context, p authz.Principal, email string, userID *uuid.UUID) ([]Ticket, error)
	CheckIn(ctx context.Context, p authz.Principal, ticketID, sessionID uuid.UUID) (CheckIn, error)
	CheckInMe(ctx context.Context, p authz.Principal, sessionID uuid.UUID) (CheckIn, error)
	CheckInGuest(ctx context.Context, sessionID uuid.UUID, email string) (CheckIn, error)
}

type service struct {
	tickets Store
	events  event.Store
	users   user.Store
	authz   authz.Authorizer
}

func NewService(tickets Store, events event.Store, az authz.Authorizer, users ...user.Store) Service {
	s := &service{tickets: tickets, events: events, authz: az}
	if len(users) > 0 {
		s.users = users[0]
	}
	return s
}

func (s *service) withEvent(ctx context.Context, t Ticket) Ticket {
	ev, err := s.events.Get(ctx, t.EventID)
	if err != nil {
		return t
	}
	res := ev.Resource()
	t.Event = &res
	return t
}

func (s *service) withEvents(ctx context.Context, tickets []Ticket) []Ticket {
	out := make([]Ticket, len(tickets))
	for i, t := range tickets {
		out[i] = s.withEvent(ctx, t)
	}
	return out
}

func (s *service) canRead(ctx context.Context, p authz.Principal, t Ticket) bool {
	if t.OwnerID != nil && p.ID == t.OwnerID.String() {
		return true
	}
	ev, err := s.events.Get(ctx, t.EventID)
	if err != nil {
		return false
	}
	return s.authz.Allow(p, authz.Resource{Type: authz.TypeTicket, OwnerTeam: ev.OwnerTeam}, authz.Read)
}

func (s *service) Apply(ctx context.Context, p authz.Principal, eventID uuid.UUID) (Ticket, error) {
	if !s.authz.Allow(p, authz.Resource{Type: authz.TypeTicket}, authz.Create) {
		return Ticket{}, ErrForbidden
	}
	if _, err := s.events.Get(ctx, eventID); err != nil {
		if errors.Is(err, event.ErrNotFound) {
			return Ticket{}, ErrNotFound
		}
		return Ticket{}, err
	}
	ownerID, err := uuid.Parse(p.ID)
	if err != nil {
		return Ticket{}, ErrInvalid
	}
	exists, err := s.tickets.ExistsOwnerEvent(ctx, ownerID, eventID)
	if err != nil {
		return Ticket{}, err
	}
	if exists {
		return Ticket{}, ErrConflict
	}
	created, err := s.tickets.Create(ctx, Ticket{
		EventID:    eventID,
		TicketType: Registered,
		OwnerID:    &ownerID,
	})
	if err != nil {
		return Ticket{}, err
	}
	return s.withEvent(ctx, created), nil
}

func (s *service) ApplyGuest(ctx context.Context, eventID uuid.UUID, g GuestInfo) (Ticket, error) {
	if g.FirstName == "" || g.LastName == "" || g.Email == "" || g.PhoneNumber == "" {
		return Ticket{}, ErrInvalid
	}
	if _, err := s.events.Get(ctx, eventID); err != nil {
		if errors.Is(err, event.ErrNotFound) {
			return Ticket{}, ErrNotFound
		}
		return Ticket{}, err
	}
	exists, err := s.tickets.ExistsGuestEvent(ctx, g.Email, eventID)
	if err != nil {
		return Ticket{}, err
	}
	if exists {
		return Ticket{}, ErrConflict
	}
	created, err := s.tickets.Create(ctx, Ticket{
		EventID:          eventID,
		TicketType:       Guest,
		GuestFirstName:   g.FirstName,
		GuestLastName:    g.LastName,
		GuestEmail:       g.Email,
		GuestPhoneNumber: g.PhoneNumber,
	})
	if err != nil {
		return Ticket{}, err
	}
	return s.withEvent(ctx, created), nil
}

func (s *service) Mine(ctx context.Context, p authz.Principal) ([]Ticket, error) {
	if !s.authz.Allow(p, authz.Resource{Type: authz.TypeTicket}, authz.ReadMe) {
		return nil, ErrForbidden
	}
	ownerID, err := uuid.Parse(p.ID)
	if err != nil {
		return nil, ErrInvalid
	}
	tickets, err := s.tickets.ListByOwner(ctx, ownerID)
	if err != nil {
		return nil, err
	}
	return s.withEvents(ctx, tickets), nil
}

func (s *service) ListByEvent(ctx context.Context, p authz.Principal, eventID uuid.UUID) ([]Ticket, error) {
	ev, err := s.events.Get(ctx, eventID)
	if err != nil {
		if errors.Is(err, event.ErrNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if !s.authz.Allow(p, authz.Resource{Type: authz.TypeTicket, OwnerTeam: ev.OwnerTeam}, authz.Read) {
		return nil, ErrForbidden
	}
	tickets, err := s.tickets.ListByEvent(ctx, eventID)
	if err != nil {
		return nil, err
	}
	return s.withEvents(ctx, tickets), nil
}

func (s *service) Get(ctx context.Context, p authz.Principal, id uuid.UUID) (Ticket, error) {
	t, err := s.tickets.Get(ctx, id)
	if err != nil {
		return Ticket{}, err
	}
	if !s.canRead(ctx, p, t) {
		return Ticket{}, ErrForbidden
	}
	return s.withEvent(ctx, t), nil
}

func (s *service) GetByUserEvent(ctx context.Context, p authz.Principal, userID, eventID uuid.UUID) (Ticket, error) {
	t, err := s.tickets.GetByOwnerEvent(ctx, userID, eventID)
	if err != nil {
		return Ticket{}, err
	}
	if !s.canRead(ctx, p, t) {
		return Ticket{}, ErrForbidden
	}
	return s.withEvent(ctx, t), nil
}

func (s *service) ListQuery(ctx context.Context, p authz.Principal, email string, userID *uuid.UUID) ([]Ticket, error) {
	email = strings.TrimSpace(email)
	if email == "" && userID == nil {
		return nil, ErrInvalid
	}

	var byUser []Ticket
	if userID != nil {
		listed, err := s.tickets.ListByOwner(ctx, *userID)
		if err != nil {
			return nil, err
		}
		byUser = listed
	}

	ownEmail := false
	var byEmail []Ticket
	if email != "" {
		if s.users != nil {
			found, err := s.users.FindByEmail(ctx, email)
			if err != nil {
				return nil, err
			}
			for _, u := range found {
				if p.ID == u.ID.String() {
					ownEmail = true
				}
				listed, err := s.tickets.ListByOwner(ctx, u.ID)
				if err != nil {
					return nil, err
				}
				byEmail = append(byEmail, listed...)
			}
		}
		guests, err := s.tickets.ListByGuestEmail(ctx, email)
		if err != nil {
			return nil, err
		}
		byEmail = append(byEmail, guests...)
	}

	var tickets []Ticket
	switch {
	case userID != nil && email != "":
		tickets = intersectTickets(byUser, byEmail)
	case userID != nil:
		tickets = byUser
	default:
		tickets = byEmail
	}

	seen := map[uuid.UUID]struct{}{}
	uniq := make([]Ticket, 0, len(tickets))
	for _, t := range tickets {
		if _, ok := seen[t.ID]; ok {
			continue
		}
		seen[t.ID] = struct{}{}
		uniq = append(uniq, t)
	}

	ownUser := userID != nil && p.ID == userID.String()
	own := ownUser
	if userID == nil && ownEmail {
		own = true
	}

	if own {
		return s.withEvents(ctx, uniq), nil
	}
	visible := make([]Ticket, 0, len(uniq))
	for _, t := range uniq {
		if s.canRead(ctx, p, t) {
			visible = append(visible, t)
		}
	}
	if len(visible) == 0 && !s.authz.Allow(p, authz.Resource{Type: authz.TypeTicket}, authz.Read) && !hasLeaderGroup(p) {
		return nil, ErrForbidden
	}
	return s.withEvents(ctx, visible), nil
}

func intersectTickets(a, b []Ticket) []Ticket {
	inB := map[uuid.UUID]struct{}{}
	for _, t := range b {
		inB[t.ID] = struct{}{}
	}
	out := make([]Ticket, 0)
	for _, t := range a {
		if _, ok := inB[t.ID]; ok {
			out = append(out, t)
		}
	}
	return out
}

func hasLeaderGroup(p authz.Principal) bool {
	for _, g := range p.Groups {
		if strings.Contains(g, "/LIDERLER") || strings.Contains(g, "/KOORDINATORLER") {
			return true
		}
		if strings.HasSuffix(g, "/YK") || strings.Contains(g, "/YK/") {
			return true
		}
		if strings.HasSuffix(g, "/DK") || strings.Contains(g, "/DK/") {
			return true
		}
		if strings.HasSuffix(g, "/ADMIN") || strings.Contains(g, "/ADMIN/") {
			return true
		}
	}
	return false
}

func (s *service) CheckIn(ctx context.Context, p authz.Principal, ticketID, sessionID uuid.UUID) (CheckIn, error) {
	t, err := s.tickets.Get(ctx, ticketID)
	if err != nil {
		return CheckIn{}, err
	}
	ev, err := s.events.Get(ctx, t.EventID)
	if err != nil {
		if errors.Is(err, event.ErrNotFound) {
			return CheckIn{}, ErrNotFound
		}
		return CheckIn{}, err
	}
	if !s.authz.Allow(p, authz.Resource{Type: authz.TypeTicket, OwnerTeam: ev.OwnerTeam}, authz.Validate) {
		return CheckIn{}, ErrForbidden
	}
	return s.addSessionCheckIn(ctx, t, sessionID)
}

func (s *service) addSessionCheckIn(ctx context.Context, t Ticket, sessionID uuid.UUID) (CheckIn, error) {
	sess, err := s.events.GetSession(ctx, sessionID)
	if err != nil {
		if errors.Is(err, event.ErrNotFound) {
			return CheckIn{}, ErrNotFound
		}
		return CheckIn{}, err
	}
	day, err := s.events.GetDay(ctx, sess.EventDayID)
	if err != nil {
		if errors.Is(err, event.ErrNotFound) {
			return CheckIn{}, ErrNotFound
		}
		return CheckIn{}, err
	}
	if day.EventID != t.EventID {
		return CheckIn{}, ErrInvalid
	}
	dup, err := s.tickets.HasCheckIn(ctx, t.ID, sessionID)
	if err != nil {
		return CheckIn{}, err
	}
	if dup {
		return CheckIn{}, ErrConflict
	}
	return s.tickets.AddCheckIn(ctx, CheckIn{TicketID: t.ID, SessionID: sessionID, EventDayID: sess.EventDayID})
}

func (s *service) ticketEventID(ctx context.Context, sessionID uuid.UUID) (uuid.UUID, error) {
	sess, err := s.events.GetSession(ctx, sessionID)
	if err != nil {
		if errors.Is(err, event.ErrNotFound) {
			return uuid.Nil, ErrNotFound
		}
		return uuid.Nil, err
	}
	day, err := s.events.GetDay(ctx, sess.EventDayID)
	if err != nil {
		if errors.Is(err, event.ErrNotFound) {
			return uuid.Nil, ErrNotFound
		}
		return uuid.Nil, err
	}
	return day.EventID, nil
}

func (s *service) CheckInMe(ctx context.Context, p authz.Principal, sessionID uuid.UUID) (CheckIn, error) {
	ownerID, err := uuid.Parse(p.ID)
	if err != nil {
		return CheckIn{}, ErrInvalid
	}
	eventID, err := s.ticketEventID(ctx, sessionID)
	if err != nil {
		return CheckIn{}, err
	}
	t, err := s.tickets.GetByOwnerEvent(ctx, ownerID, eventID)
	if err != nil {
		return CheckIn{}, err
	}
	return s.addSessionCheckIn(ctx, t, sessionID)
}

func (s *service) CheckInGuest(ctx context.Context, sessionID uuid.UUID, email string) (CheckIn, error) {
	email = strings.TrimSpace(email)
	if email == "" {
		return CheckIn{}, ErrInvalid
	}
	eventID, err := s.ticketEventID(ctx, sessionID)
	if err != nil {
		return CheckIn{}, err
	}
	listed, err := s.tickets.ListByGuestEmail(ctx, email)
	if err != nil {
		return CheckIn{}, err
	}
	for _, t := range listed {
		if t.EventID == eventID {
			return s.addSessionCheckIn(ctx, t, sessionID)
		}
	}
	return CheckIn{}, ErrNotFound
}
