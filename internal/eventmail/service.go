package eventmail

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/mail"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

var (
	ErrForbidden    = errors.New("eventmail: forbidden")
	ErrNotFound     = errors.New("eventmail: not found")
	ErrNoRecipients = errors.New("eventmail: no recipients")
	ErrUnavailable  = errors.New("eventmail: unavailable")
)

type Result struct {
	MailListID     uuid.UUID `json:"mailListId"`
	Name           string    `json:"name"`
	RecipientCount int       `json:"recipientCount"`
}

type Service interface {
	Sync(ctx context.Context, p authz.Principal, eventID uuid.UUID, ticketIDs []uuid.UUID) (Result, error)
}

type service struct {
	events    event.Store
	tickets   ticket.Store
	users     user.Store
	lists     mail.Lists
	authz     authz.Authorizer
	snapshots SnapshotStore
}

func New(events event.Store, tickets ticket.Store, users user.Store, lists mail.Lists, az authz.Authorizer, snapshots ...SnapshotStore) Service {
	var snapshotStore SnapshotStore
	if len(snapshots) > 0 {
		snapshotStore = snapshots[0]
	}
	return &service{events: events, tickets: tickets, users: users, lists: lists, authz: az, snapshots: snapshotStore}
}

func (s *service) Sync(ctx context.Context, p authz.Principal, eventID uuid.UUID, ticketIDs []uuid.UUID) (Result, error) {
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
	if ticketIDs != nil {
		selected := make(map[uuid.UUID]struct{}, len(ticketIDs))
		for _, id := range ticketIDs {
			selected[id] = struct{}{}
		}
		filtered := make([]ticket.Ticket, 0, len(selected))
		for _, row := range rows {
			if _, ok := selected[row.ID]; ok {
				filtered = append(filtered, row)
			}
		}
		rows = filtered
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
	var listID uuid.UUID
	var have []mail.ListRecipient
	if ticketIDs != nil {
		if len(want) == 0 {
			return Result{}, ErrNoRecipients
		}
		if s.snapshots == nil {
			return Result{}, ErrUnavailable
		}
		name = fmt.Sprintf("%s · Seçili %d · %s", name, len(want), time.Now().Format("2006-01-02 15:04"))
		listID, err = s.lists.CreateList(ctx, name)
		if err == nil {
			err = s.snapshots.Track(ctx, Snapshot{
				MailListID: listID,
				EventID:    ev.ID,
				ExpiresAt:  time.Now().UTC().Add(SnapshotRetention),
			})
			if err != nil {
				if cleanupErr := s.lists.DeleteList(ctx, listID); cleanupErr != nil && !errors.Is(cleanupErr, mail.ErrListNotFound) {
					err = errors.Join(err, cleanupErr)
				}
			}
		}
	} else {
		listID, err = s.ensureList(ctx, ev, name)
		if err == nil {
			have, err = s.lists.Recipients(ctx, listID)
		}
	}
	if err != nil {
		return Result{}, err
	}
	wanted := map[string]Recipient{}
	for _, r := range want {
		wanted[r.Email] = r
	}
	haveByEmail := map[string]mail.ListRecipient{}
	if ticketIDs == nil {
		for _, r := range have {
			email := strings.ToLower(strings.TrimSpace(r.Email))
			haveByEmail[email] = r
			if _, ok := wanted[email]; !ok {
				if err := s.lists.RemoveRecipient(ctx, listID, r.ID); err != nil {
					return Result{}, err
				}
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
