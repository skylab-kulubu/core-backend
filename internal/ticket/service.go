package ticket

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/event"
)

type Service interface {
	Apply(ctx context.Context, p authz.Principal, eventID uuid.UUID) (Ticket, error)
	ApplyGuest(ctx context.Context, eventID uuid.UUID, g GuestInfo) (Ticket, error)
	Mine(ctx context.Context, p authz.Principal) ([]Ticket, error)
	CheckIn(ctx context.Context, p authz.Principal, ticketID, eventDayID uuid.UUID) (CheckIn, error)
}

type service struct {
	tickets Store
	events  event.Store
	authz   authz.Authorizer
}

func NewService(tickets Store, events event.Store, az authz.Authorizer) Service {
	return &service{tickets: tickets, events: events, authz: az}
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
	return s.tickets.Create(ctx, Ticket{
		EventID:    eventID,
		TicketType: Registered,
		OwnerID:    &ownerID,
	})
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
	return s.tickets.Create(ctx, Ticket{
		EventID:          eventID,
		TicketType:       Guest,
		GuestFirstName:   g.FirstName,
		GuestLastName:    g.LastName,
		GuestEmail:       g.Email,
		GuestPhoneNumber: g.PhoneNumber,
	})
}

func (s *service) Mine(ctx context.Context, p authz.Principal) ([]Ticket, error) {
	if !s.authz.Allow(p, authz.Resource{Type: authz.TypeTicket}, authz.ReadMe) {
		return nil, ErrForbidden
	}
	ownerID, err := uuid.Parse(p.ID)
	if err != nil {
		return nil, ErrInvalid
	}
	return s.tickets.ListByOwner(ctx, ownerID)
}

func (s *service) CheckIn(ctx context.Context, p authz.Principal, ticketID, eventDayID uuid.UUID) (CheckIn, error) {
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
	if !s.authz.Allow(p, authz.Resource{Type: authz.TypeTicket, OwnerTeam: ev.OwnerTeam, EventType: ev.OwnerTeam}, authz.Validate) {
		return CheckIn{}, ErrForbidden
	}
	day, err := s.events.GetDay(ctx, eventDayID)
	if err != nil {
		if errors.Is(err, event.ErrNotFound) {
			return CheckIn{}, ErrNotFound
		}
		return CheckIn{}, err
	}
	if day.EventID != t.EventID {
		return CheckIn{}, ErrInvalid
	}
	dup, err := s.tickets.HasCheckIn(ctx, ticketID, eventDayID)
	if err != nil {
		return CheckIn{}, err
	}
	if dup {
		return CheckIn{}, ErrConflict
	}
	return s.tickets.AddCheckIn(ctx, CheckIn{TicketID: ticketID, EventDayID: eventDayID})
}
