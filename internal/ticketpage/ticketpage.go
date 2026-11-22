// Package ticketpage answers the ticket page's public routes: the Ticket an
// access key opens, as JSON, its QR as a PNG and the PDF ticket (ADR 0082;
// docs/ticket-page.md). The page itself is the club site's; core gives it
// the least it needs, and no e-mail, phone, Sky number or ticket type.
package ticketpage

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/qr"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
	"github.com/skylab-kulubu/core-backend/internal/ticket/theme"
	"github.com/skylab-kulubu/core-backend/internal/ticketqr"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

var (
	// ErrNotFound is a key no current Ticket with a holder has, a Ticket of
	// a draft or archived Event, or a key that is not one.
	ErrNotFound = errors.New("ticketpage: not found")
	// ErrGone is a page that has closed (its Event ended more than the
	// grace ago), or a QR or PDF of a cancelled Event.
	ErrGone = errors.New("ticketpage: gone")
	// ErrUnavailable is a PDF that cannot be drawn now (no renderer, or it
	// is busy).
	ErrUnavailable = errors.New("ticketpage: unavailable")
)

// The page's states.
const (
	StateActive         = "active"
	StateEventCancelled = "event_cancelled"
	StateEnded          = "ended"
)

// QRSize is the QR PNG's side in pixels.
const QRSize = 512

// Renderer turns the ticket's HTML into a tagged PDF (core's Gotenberg).
type Renderer interface {
	TaggedPDF(ctx context.Context, html string) ([]byte, error)
}

// TeamLogos reads an organizing team's logo for the seal: an image and its
// type, or ok false when the team has none (the seal is then the club's
// alone).
type TeamLogos interface {
	TeamLogo(ctx context.Context, team string) (data []byte, contentType string, ok bool)
}

// Config is what the routes need besides the stores.
type Config struct {
	Page ticket.PageConfig
	// APIOrigin is core's public origin (PUBLIC_API_ORIGIN): the QR and PDF
	// links the JSON gives. Empty gives paths.
	APIOrigin string
	// RenderSlots bounds the PDFs drawn at once; RenderWait is how long a
	// request waits for a slot before it is told to try again.
	RenderSlots int
	RenderWait  time.Duration
}

// Service answers the three routes.
type Service struct {
	tickets ticket.Store
	events  event.Store
	users   user.Store
	qrs     ticket.TicketQRs
	render  Renderer
	logos   TeamLogos
	cfg     Config
	slots   chan struct{}
	// Now is the clock; nil is time.Now.
	Now func() time.Time
}

// New builds the service. render and logos may be nil: no PDF, and a seal
// without the team's logo.
func New(tickets ticket.Store, events event.Store, users user.Store, qrs ticket.TicketQRs, render Renderer, logos TeamLogos, cfg Config) *Service {
	if cfg.RenderSlots < 1 {
		cfg.RenderSlots = 2
	}
	if cfg.RenderWait <= 0 {
		cfg.RenderWait = 20 * time.Second
	}
	return &Service{
		tickets: tickets, events: events, users: users, qrs: qrs, render: render, logos: logos,
		cfg: cfg, slots: make(chan struct{}, cfg.RenderSlots),
	}
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// Theme is the look the JSON tells the site (ticket 16 adds the Event's
// choice and the team's logo).
type Theme struct {
	Key    string `json:"key"`
	Accent string `json:"accent"`
}

// View is the JSON the ticket page reads.
type View struct {
	EventName   string     `json:"eventName"`
	EventStart  *time.Time `json:"eventStart"`
	EventEnd    *time.Time `json:"eventEnd"`
	Venue       string     `json:"venue"`
	OwnerTeam   string     `json:"ownerTeam"`
	EventStatus string     `json:"eventStatus"`
	HolderName  string     `json:"holderName"`
	Code        string     `json:"code"`
	QRToken     string     `json:"qrToken"`
	QRURL       string     `json:"qrUrl"`
	PDFURL      string     `json:"pdfUrl"`
	State       string     `json:"state"`
	Theme       Theme      `json:"theme"`
}

// opened is a Ticket the key opened, with its Event and holder.
type opened struct {
	ticket ticket.Ticket
	event  event.Event
	holder string
	state  string
}

func (s *Service) open(ctx context.Context, key string) (opened, error) {
	if !ticket.ValidAccessKey(key) {
		return opened{}, ErrNotFound
	}
	t, err := s.tickets.GetByAccessKey(ctx, key)
	if errors.Is(err, ticket.ErrNotFound) {
		return opened{}, ErrNotFound
	}
	if err != nil {
		return opened{}, err
	}
	if !ticket.Held(t) {
		return opened{}, ErrNotFound
	}
	ev, err := s.events.Get(ctx, t.EventID)
	if errors.Is(err, event.ErrNotFound) {
		return opened{}, ErrNotFound
	}
	if err != nil {
		return opened{}, err
	}
	// A draft does not exist for anyone signed out (ADR 0066).
	if ev.Status == event.StatusDraft {
		return opened{}, ErrNotFound
	}
	now := s.now()
	if s.cfg.Page.Closed(ev, now) {
		return opened{}, ErrGone
	}
	holder, err := s.holderName(ctx, t)
	if err != nil {
		return opened{}, err
	}
	state := StateActive
	switch {
	case ev.Status == event.StatusCancelled:
		state = StateEventCancelled
	case ev.EndedAt(now):
		state = StateEnded
	}
	return opened{ticket: t, event: ev, holder: holder, state: state}, nil
}

// holderName is the name the ticket shows: the guest's, or the member's
// from core's row. A member whose account is no longer active has no page.
func (s *Service) holderName(ctx context.Context, t ticket.Ticket) (string, error) {
	if t.TicketType == ticket.Guest {
		return strings.TrimSpace(t.GuestFirstName + " " + t.GuestLastName), nil
	}
	if s.users == nil || t.OwnerID == nil {
		return "", ErrNotFound
	}
	u, err := s.users.Get(ctx, *t.OwnerID)
	if errors.Is(err, user.ErrNotFound) || errors.Is(err, user.ErrAccountBlocked) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}
	if u.AccountState != "" && u.AccountState != user.AccountActive {
		return "", ErrNotFound
	}
	return strings.TrimSpace(u.FirstName + " " + u.LastName), nil
}

func (s *Service) link(key, suffix string) string {
	return strings.TrimRight(s.cfg.APIOrigin, "/") + "/v1/public/tickets/" + key + suffix
}

// Page is the JSON of the Ticket key opens. A cancelled Event's answer
// keeps the Ticket but gives no QR and no PDF.
func (s *Service) Page(ctx context.Context, key string) (View, error) {
	o, err := s.open(ctx, key)
	if err != nil {
		return View{}, err
	}
	th := theme.Lookup("")
	v := View{
		EventName: o.event.Name, EventStart: o.event.StartDate, EventEnd: o.event.EndDate,
		Venue: o.event.Location, OwnerTeam: o.event.OwnerTeam, EventStatus: string(o.event.Status),
		HolderName: o.holder, Code: o.ticket.Code, State: o.state,
		Theme: Theme{Key: th.Key, Accent: th.Accent},
	}
	if o.state != StateEventCancelled {
		token, err := s.token(o)
		if err != nil {
			return View{}, err
		}
		v.QRToken = token
		v.QRURL = s.link(key, "/qr.png")
		v.PDFURL = s.link(key, "/pdf")
	}
	return v, nil
}

func (s *Service) token(o opened) (string, error) {
	if s.qrs == nil {
		return "", ErrUnavailable
	}
	return s.qrs.Mint(ticketQRClaims(o.ticket, o.event))
}

// QRPNG is the Ticket QR as a PNG: error correction M, a four-module quiet
// zone, black on white, QRSize pixels.
func (s *Service) QRPNG(ctx context.Context, key string) ([]byte, error) {
	o, err := s.open(ctx, key)
	if err != nil {
		return nil, err
	}
	if o.state == StateEventCancelled {
		return nil, ErrGone
	}
	token, err := s.token(o)
	if err != nil {
		return nil, err
	}
	return qr.PNG(token, QRSize)
}

// PDF is the ticket as a one-page PDF and its file name, drawn now and never
// kept.
func (s *Service) PDF(ctx context.Context, key string) (string, []byte, error) {
	o, err := s.open(ctx, key)
	if err != nil {
		return "", nil, err
	}
	if o.state == StateEventCancelled {
		return "", nil, ErrGone
	}
	if s.render == nil {
		return "", nil, ErrUnavailable
	}
	token, err := s.token(o)
	if err != nil {
		return "", nil, err
	}
	doc, err := s.document(ctx, o, token)
	if err != nil {
		return "", nil, err
	}
	html, err := RenderHTML(doc)
	if err != nil {
		return "", nil, err
	}
	wait, cancel := context.WithTimeout(ctx, s.cfg.RenderWait)
	defer cancel()
	select {
	case s.slots <- struct{}{}:
	case <-wait.Done():
		return "", nil, ErrUnavailable
	}
	defer func() { <-s.slots }()
	pdf, err := s.render.TaggedPDF(ctx, html)
	if err != nil {
		return "", nil, err
	}
	return FileName(o.event.Name), pdf, nil
}

func (s *Service) document(ctx context.Context, o opened, token string) (Document, error) {
	doc := Document{
		EventName: o.event.Name, Start: o.event.StartDate, End: o.event.EndDate,
		Venue: o.event.Location, OwnerTeam: o.event.OwnerTeam, HolderName: o.holder,
		Code: o.ticket.Code, QRToken: token, Theme: theme.Lookup(""), IssuedAt: s.now(),
	}
	if s.logos != nil && o.event.OwnerTeam != "" {
		if data, contentType, ok := s.logos.TeamLogo(ctx, o.event.OwnerTeam); ok {
			doc.TeamLogo, doc.TeamLogoType = data, contentType
		}
	}
	return doc, nil
}

// LogKey is how a log line names an access key: its first four characters
// (the certificate serial's pattern).
func LogKey(key string) string {
	if len(key) <= 4 {
		return "…"
	}
	return key[:4] + "…"
}

func ticketQRClaims(t ticket.Ticket, ev event.Event) ticketqr.Claims {
	return ticketqr.Claims{Ticket: t.ID, Event: ev.ID, Version: t.QRVersion, ExpiresAt: ticket.QRExpiry(t, ev)}
}
