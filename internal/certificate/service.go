package certificate

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"strings"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/qr"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

type service struct {
	store   Store
	tickets ticket.Store
	events  event.Store
	users   user.Store
	authz   authz.Authorizer
	render  Renderer
	mail    Mailer
	origin  string
}

func NewService(store Store, tickets ticket.Store, events event.Store, users user.Store, az authz.Authorizer, render Renderer, mail Mailer, publicOrigin string) Service {
	origin := strings.TrimRight(publicOrigin, "/")
	if origin == "" {
		origin = strings.TrimRight(os.Getenv("PUBLIC_API_ORIGIN"), "/")
	}
	if origin == "" {
		origin = "https://api.yildizskylab.com"
	}
	return &service{
		store: store, tickets: tickets, events: events, users: users, authz: az,
		render: render, mail: mail, origin: origin,
	}
}

func (s *service) canIssue(p authz.Principal, ownerTeam string) bool {
	return s.authz.Allow(p, authz.Resource{Type: authz.TypeCertificate, OwnerTeam: ownerTeam}, authz.Issue)
}

func (s *service) scheduledAndCheckIns(ctx context.Context, t ticket.Ticket) (scheduled, checkIns int, err error) {
	days, err := s.events.ListDays(ctx, t.EventID)
	if err != nil {
		return 0, 0, err
	}
	live := map[uuid.UUID]struct{}{}
	for _, day := range days {
		sessions, err := s.events.ListSessions(ctx, day.ID)
		if err != nil {
			return 0, 0, err
		}
		for _, sess := range sessions {
			if sess.Cancelled {
				continue
			}
			live[sess.ID] = struct{}{}
			scheduled++
		}
	}
	for _, ci := range t.CheckIns {
		if _, ok := live[ci.SessionID]; ok {
			checkIns++
		}
	}
	return scheduled, checkIns, nil
}

func (s *service) recipient(ctx context.Context, t ticket.Ticket) (name, email string, err error) {
	if t.TicketType == ticket.Guest {
		name = strings.TrimSpace(t.GuestFirstName + " " + t.GuestLastName)
		email = t.GuestEmail
		if name == "" || email == "" {
			return "", "", ErrInvalid
		}
		return name, email, nil
	}
	if t.OwnerID == nil || s.users == nil {
		return "", "", ErrInvalid
	}
	u, err := s.users.Get(ctx, *t.OwnerID)
	if err != nil {
		if errors.Is(err, user.ErrNotFound) {
			return "", "", ErrInvalid
		}
		return "", "", err
	}
	name = strings.TrimSpace(u.FirstName + " " + u.LastName)
	email = u.Email
	if name == "" || email == "" {
		return "", "", ErrInvalid
	}
	return name, email, nil
}

func newSerial() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return strings.ToUpper(hex.EncodeToString(b[:])), nil
}

func (s *service) verifyURL(serial string) string {
	return s.origin + "/v1/certificates/verify/" + serial
}

func (s *service) materialize(ctx context.Context, ev event.Event, t ticket.Ticket) (Certificate, error) {
	if _, err := s.store.GetActive(ctx, ev.ID, t.ID); err == nil {
		return Certificate{}, ErrConflict
	} else if err != nil && !errors.Is(err, ErrNotFound) {
		return Certificate{}, err
	}
	name, email, err := s.recipient(ctx, t)
	if err != nil {
		return Certificate{}, err
	}
	serial, err := newSerial()
	if err != nil {
		return Certificate{}, err
	}
	url := s.verifyURL(serial)
	qrPNG, err := qr.PNG(url, qr.DefaultSize)
	if err != nil {
		return Certificate{}, err
	}
	html := HTML(ev.OwnerTeam, name, ev.Name, url, qrPNG)
	if s.render == nil {
		return Certificate{}, ErrInvalid
	}
	pdf, err := s.render.PDF(ctx, html)
	if err != nil {
		return Certificate{}, err
	}
	c := Certificate{
		EventID:        ev.ID,
		TicketID:       t.ID,
		OwnerID:        t.OwnerID,
		Serial:         serial,
		RecipientName:  name,
		RecipientEmail: email,
		EventName:      ev.Name,
		OwnerTeam:      ev.OwnerTeam,
		VerifyURL:      url,
	}
	created, err := s.store.Create(ctx, c, pdf)
	if err != nil {
		return Certificate{}, err
	}
	if s.mail != nil {
		first := name
		if parts := strings.Fields(name); len(parts) > 0 {
			first = parts[0]
		}
		s.mail.Certificate(ctx, email, name, map[string]string{
			"FirstName": first,
			"EventName": ev.Name,
			"VerifyURL": url,
			"Serial":    serial,
			"OwnerTeam": ev.OwnerTeam,
		})
	}
	return created, nil
}

func (s *service) RecomputeTicket(ctx context.Context, ticketID uuid.UUID) (*Certificate, error) {
	t, err := s.tickets.Get(ctx, ticketID)
	if err != nil {
		if errors.Is(err, ticket.ErrNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	ev, err := s.events.Get(ctx, t.EventID)
	if err != nil {
		if errors.Is(err, event.ErrNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	scheduled, checkIns, err := s.scheduledAndCheckIns(ctx, t)
	if err != nil {
		return nil, err
	}
	ratio := 0.0
	if ev.AttendanceRatio != nil {
		ratio = *ev.AttendanceRatio
	}
	if !Eligible(ev.AttendanceRule, ratio, checkIns, scheduled) {
		return nil, nil
	}
	if _, err := s.store.GetActive(ctx, ev.ID, t.ID); err == nil {
		return nil, nil
	} else if err != nil && !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	created, err := s.materialize(ctx, ev, t)
	if err != nil {
		return nil, err
	}
	return &created, nil
}

func (s *service) RecomputeEvent(ctx context.Context, p authz.Principal, eventID uuid.UUID) ([]Certificate, error) {
	ev, err := s.events.Get(ctx, eventID)
	if err != nil {
		if errors.Is(err, event.ErrNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if !s.canIssue(p, ev.OwnerTeam) {
		return nil, ErrForbidden
	}
	tickets, err := s.tickets.ListByEvent(ctx, eventID)
	if err != nil {
		return nil, err
	}
	out := make([]Certificate, 0)
	for _, t := range tickets {
		got, err := s.RecomputeTicket(ctx, t.ID)
		if err != nil {
			return nil, err
		}
		if got != nil {
			out = append(out, *got)
		}
	}
	return out, nil
}

func (s *service) Issue(ctx context.Context, p authz.Principal, eventID, ticketID uuid.UUID) (Certificate, error) {
	ev, err := s.events.Get(ctx, eventID)
	if err != nil {
		if errors.Is(err, event.ErrNotFound) {
			return Certificate{}, ErrNotFound
		}
		return Certificate{}, err
	}
	if !s.canIssue(p, ev.OwnerTeam) {
		return Certificate{}, ErrForbidden
	}
	t, err := s.tickets.Get(ctx, ticketID)
	if err != nil {
		if errors.Is(err, ticket.ErrNotFound) {
			return Certificate{}, ErrNotFound
		}
		return Certificate{}, err
	}
	if t.EventID != eventID {
		return Certificate{}, ErrInvalid
	}
	return s.materialize(ctx, ev, t)
}

func (s *service) Revoke(ctx context.Context, p authz.Principal, serial string) error {
	c, _, err := s.store.GetBySerial(ctx, serial)
	if err != nil {
		return err
	}
	if !s.authz.Allow(p, authz.Resource{Type: authz.TypeCertificate, OwnerTeam: c.OwnerTeam}, authz.Revoke) {
		return ErrForbidden
	}
	_, err = s.store.Revoke(ctx, serial)
	return err
}

func (s *service) Verify(ctx context.Context, serial string) (Certificate, error) {
	c, _, err := s.store.GetBySerial(ctx, serial)
	return c, err
}

func (s *service) PDF(ctx context.Context, serial string) ([]byte, error) {
	c, pdf, err := s.store.GetBySerial(ctx, serial)
	if err != nil {
		return nil, err
	}
	if c.RevokedAt != nil {
		return nil, ErrNotFound
	}
	return pdf, nil
}

func (s *service) Mine(ctx context.Context, p authz.Principal) ([]Certificate, error) {
	id, err := uuid.Parse(p.ID)
	if err != nil {
		return nil, ErrInvalid
	}
	return s.store.ListByOwner(ctx, id)
}

func (s *service) ListByEvent(ctx context.Context, p authz.Principal, eventID uuid.UUID) ([]Certificate, error) {
	ev, err := s.events.Get(ctx, eventID)
	if err != nil {
		if errors.Is(err, event.ErrNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if !s.authz.Allow(p, authz.Resource{Type: authz.TypeCertificate, OwnerTeam: ev.OwnerTeam}, authz.Read) {
		return nil, ErrForbidden
	}
	return s.store.ListByEvent(ctx, eventID)
}
