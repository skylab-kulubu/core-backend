package eventmail

import (
	"context"
	"errors"
	"strings"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/mail"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

var (
	ErrForbidden   = errors.New("eventmail: forbidden")
	ErrNotFound    = errors.New("eventmail: not found")
	ErrUnavailable = errors.New("eventmail: unavailable")
)

type Result struct {
	MailListID     uuid.UUID `json:"mailListId"`
	Name           string    `json:"name"`
	RecipientCount int       `json:"recipientCount"`
}

type Service interface {
	Sync(ctx context.Context, p authz.Principal, eventID uuid.UUID) (Result, error)
}

type service struct {
	events  event.Store
	tickets ticket.Store
	users   user.Store
	lists   mail.Lists
	authz   authz.Authorizer
}

func New(events event.Store, tickets ticket.Store, users user.Store, lists mail.Lists, az authz.Authorizer) Service {
	return &service{events: events, tickets: tickets, users: users, lists: lists, authz: az}
}

func (s *service) Sync(ctx context.Context, p authz.Principal, eventID uuid.UUID) (Result, error) {
	if s.lists == nil {
		return Result{}, ErrUnavailable
	}
	ev, err := s.events.Get(ctx, eventID)
	if err != nil {
		if errors.Is(err, event.ErrNotFound) {
			return Result{}, ErrNotFound
		}
		return Result{}, err
	}
	if !s.authz.Allow(p, authz.Resource{Type: authz.TypeTicket, OwnerTeam: ev.OwnerTeam}, authz.Read) {
		return Result{}, ErrForbidden
	}
	rows, err := s.tickets.ListByEvent(ctx, eventID)
	if err != nil {
		return Result{}, err
	}
	people := map[uuid.UUID]user.User{}
	for _, row := range rows {
		if row.OwnerID == nil {
			continue
		}
		if _, ok := people[*row.OwnerID]; ok {
			continue
		}
		u, err := s.users.Get(ctx, *row.OwnerID)
		if err != nil {
			if errors.Is(err, user.ErrNotFound) {
				continue
			}
			return Result{}, err
		}
		people[u.ID] = u
	}
	want := RecipientsFromTickets(rows, people)
	name := ListName(ev.OwnerTeam, ev.Name)
	listID, err := s.ensureList(ctx, ev, name)
	if err != nil {
		return Result{}, err
	}
	have, err := s.lists.Recipients(ctx, listID)
	if err != nil {
		return Result{}, err
	}
	wanted := map[string]Recipient{}
	for _, r := range want {
		wanted[r.Email] = r
	}
	haveByEmail := map[string]mail.ListRecipient{}
	for _, r := range have {
		email := strings.ToLower(strings.TrimSpace(r.Email))
		haveByEmail[email] = r
		if _, ok := wanted[email]; !ok {
			if err := s.lists.RemoveRecipient(ctx, listID, r.ID); err != nil {
				return Result{}, err
			}
		}
	}
	for _, r := range want {
		if _, ok := haveByEmail[r.Email]; ok {
			continue
		}
		if err := s.lists.AddRecipient(ctx, listID, mail.ListRecipient{FullName: r.FullName, Email: r.Email}); err != nil {
			return Result{}, err
		}
	}
	return Result{MailListID: listID, Name: name, RecipientCount: len(want)}, nil
}

func (s *service) ensureList(ctx context.Context, ev event.Event, name string) (uuid.UUID, error) {
	if ev.MailListID != nil && *ev.MailListID != uuid.Nil {
		err := s.lists.GetList(ctx, *ev.MailListID)
		if err == nil {
			return *ev.MailListID, nil
		}
		if !errors.Is(err, mail.ErrListNotFound) {
			return uuid.Nil, err
		}
	}
	id, err := s.lists.CreateList(ctx, name)
	if err != nil {
		return uuid.Nil, err
	}
	if _, err := s.events.SetMailListID(ctx, ev.ID, id); err != nil {
		return uuid.Nil, err
	}
	return id, nil
}
