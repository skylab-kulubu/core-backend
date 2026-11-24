// Package ticketpage answers the ticket page's public routes: the Ticket an
// access key opens, as JSON, its QR as a PNG and the PDF ticket (ADR 0082;
// docs/ticket-page.md). The page itself is the club site's; core gives it
// the least it needs, and no e-mail, phone, Sky number or ticket type.
package ticketpage

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/media"

	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/organizer"
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

// Renderer turns the ticket's HTML into a tagged PDF, header drawn at the
// top of every page (core's Gotenberg).
type Renderer interface {
	TaggedPDF(ctx context.Context, html, header string) ([]byte, error)
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
	images  Images
	bands   bands
	cfg     Config
	slots   chan struct{}
	// Now is the clock; nil is time.Now.
	Now func() time.Time
	// Entries reads what the page polls on the Event's day (CheckIns):
	// core's Postgres store. Nil answers every key 404.
	Entries EntryStore
}

// EntryStore reads a Ticket's own check-ins by its key in one query
// (ticket.PostgresStore).
type EntryStore interface {
	PageCheckIns(ctx context.Context, key string) (ticket.PageCheckIns, error)
}

// New builds the service. render and images may be nil: no PDF, and a PDF
// without the cover's picture (its band is then the cover's colour).
func New(tickets ticket.Store, events event.Store, users user.Store, qrs ticket.TicketQRs, render Renderer, images Images, cfg Config) *Service {
	if cfg.RenderSlots < 1 {
		cfg.RenderSlots = 2
	}
	if cfg.RenderWait <= 0 {
		cfg.RenderWait = 20 * time.Second
	}
	return &Service{
		tickets: tickets, events: events, users: users, qrs: qrs, render: render, images: images,
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
// choice).
type Theme struct {
	Key    string `json:"key"`
	Accent string `json:"accent"`
}

// SiteAccent is the club's purple: the ticket page's accent when the Event
// gives none (Yusuf's order: the Event's ticket colour, then its cover's
// first colour, then this).
const SiteAccent = "#8B5CF6"

// Accent is the ticket page's accent for the Event: its cover's first
// colour, else the club's purple. (The Event's own ticket colour,
// ticketAccentColor, goes first once Events have one: ticket 16.)
func Accent(ev event.Event) string {
	for _, c := range ev.CoverColors {
		if validColor(c) {
			return strings.ToUpper(c)
		}
	}
	return SiteAccent
}

// View is the JSON the ticket page reads.
type View struct {
	EventName  string     `json:"eventName"`
	EventStart *time.Time `json:"eventStart"`
	EventEnd   *time.Time `json:"eventEnd"`
	Venue      string     `json:"venue"`
	// VenueAddress and the point are the place's (docs/event-location.md),
	// left out while it has none; DirectionsURL and MapsURL are its links
	// for "Yol tarifi" and "Haritada aç".
	// CoverImageURL is the Event's cover (a public media address), left
	// out while it has none; CoverColors its colours, first the dominant
	// one (empty without a cover).
	CoverImageURL string   `json:"coverImageUrl,omitempty"`
	CoverColors   []string `json:"coverColors"`
	VenueAddress  string   `json:"venueAddress,omitempty"`
	VenueLat      *float64 `json:"venueLat,omitempty"`
	VenueLng      *float64 `json:"venueLng,omitempty"`
	DirectionsURL string   `json:"directionsUrl,omitempty"`
	MapsURL       string   `json:"mapsUrl,omitempty"`
	// MapImageURL is the map of the place's point core drew
	// (docs/event-map.md), MapAttribution the credit to show under it,
	// linked to MapAttributionURL; all left out while there is none.
	MapImageURL       string `json:"mapImageUrl,omitempty"`
	MapAttribution    string `json:"mapAttribution,omitempty"`
	MapAttributionURL string `json:"mapAttributionUrl,omitempty"`
	OwnerTeam         string `json:"ownerTeam"`
	OrganizerName     string `json:"organizerName"` // organizer.Name(OwnerTeam): the club for YK, DK, ADMIN, none
	EventStatus       string `json:"eventStatus"`
	HolderName        string `json:"holderName"`
	Code              string `json:"code"`
	QRToken           string `json:"qrToken"`
	QRURL             string `json:"qrUrl"`
	PDFURL            string `json:"pdfUrl"`
	State             string `json:"state"`
	Theme             Theme  `json:"theme"`
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
	ev := o.event
	v := View{
		EventName: ev.Name, EventStart: ev.StartDate, EventEnd: ev.EndDate,
		Venue: ev.Location, VenueAddress: ev.LocationAddress, VenueLat: ev.LocationLat, VenueLng: ev.LocationLng,
		DirectionsURL: DirectionsURL(ev.Location, ev.LocationAddress, ev.LocationLat, ev.LocationLng),
		MapsURL:       MapsURL(ev.Location, ev.LocationAddress, ev.LocationLat, ev.LocationLng),
		MapImageURL:   media.ConfiguredAddresses().Object(ev.MapImageURL),
		OwnerTeam:     ev.OwnerTeam, OrganizerName: organizer.Name(ev.OwnerTeam), EventStatus: string(ev.Status),
		HolderName: o.holder, Code: o.ticket.Code, State: o.state,
		Theme:         Theme{Key: th.Key, Accent: Accent(ev)},
		CoverImageURL: media.ConfiguredAddresses().Object(ev.CoverImageURL),
		CoverColors:   append([]string{}, ev.CoverColors...),
	}
	if v.MapImageURL != "" {
		v.MapAttribution, v.MapAttributionURL = event.MapAttribution, event.MapAttributionURL
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

// CheckInsView is the JSON of GET /v1/public/tickets/{key}/check-ins: the
// Ticket's own check-ins, the Session's title and when, oldest first.
type CheckInsView struct {
	CheckIns []ticket.PageCheckIn `json:"checkIns"`
}

// CheckIns is what the ticket page polls while it is open on the Event's
// day, to greet its holder at the door. The page's own rules, in one query
// rather than the page's several: 404 for an unknown key, a Ticket without
// its holder, a draft or archived Event; 410 once the page has closed.
func (s *Service) CheckIns(ctx context.Context, key string) (CheckInsView, error) {
	if !ticket.ValidAccessKey(key) || s.Entries == nil {
		return CheckInsView{}, ErrNotFound
	}
	got, err := s.Entries.PageCheckIns(ctx, key)
	if errors.Is(err, ticket.ErrNotFound) {
		return CheckInsView{}, ErrNotFound
	}
	if err != nil {
		return CheckInsView{}, err
	}
	if !got.Held || got.Event.Status == event.StatusDraft {
		return CheckInsView{}, ErrNotFound
	}
	if s.cfg.Page.Closed(got.Event, s.now()) {
		return CheckInsView{}, ErrGone
	}
	checkIns := got.CheckIns
	if checkIns == nil {
		checkIns = []ticket.PageCheckIn{}
	}
	return CheckInsView{CheckIns: checkIns}, nil
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

// PDF is the ticket as a PDF (one A4 page, more for a long programme or
// description) and its file name, drawn now and never kept.
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
	html, header, err := RenderHTML(doc)
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
	pdf, err := s.render.TaggedPDF(ctx, html, header)
	if err != nil {
		// Gotenberg unreachable, timed out or failing: try again later
		// (503), the cause for the log.
		return "", nil, fmt.Errorf("%w: renderer: %w", ErrUnavailable, err)
	}
	return FileName(o.event.Name), pdf, nil
}

func (s *Service) document(ctx context.Context, o opened, token string) (Document, error) {
	ev := o.event
	th := theme.Lookup("")
	doc := Document{
		EventName: ev.Name, Start: ev.StartDate, End: ev.EndDate,
		Venue: ev.Location, VenueAddress: ev.LocationAddress,
		DirectionsURL: DirectionsURL(ev.Location, ev.LocationAddress, ev.LocationLat, ev.LocationLng),
		OwnerTeam:     ev.OwnerTeam, HolderName: o.holder, Code: o.ticket.Code, QRToken: token,
		Description: ev.Description, Theme: th, IssuedAt: s.now(),
	}
	program, err := s.program(ctx, ev)
	if err != nil {
		return Document{}, err
	}
	doc.Days = program
	if len(ev.CoverColors) > 0 {
		doc.BandColor = ev.CoverColors[0]
	}
	if ev.CoverImageID != nil {
		doc.BandJPEG = s.band(ctx, *ev.CoverImageID)
	}
	if ev.MapImageID != nil && s.images != nil {
		if data, contentType, ok := s.images.Image(ctx, *ev.MapImageID, ""); ok {
			doc.Map, doc.MapType = data, contentType
			doc.MapAttribution = event.MapAttribution + " · " + strings.TrimPrefix(event.MapAttributionURL, "https://www.")
		}
	}
	return doc, nil
}

// program is the Event's current days and their current Sessions, in time
// order: a cancelled Session is left out, and so is a day left without any.
// A day without a date takes its first Session's.
func (s *Service) program(ctx context.Context, ev event.Event) ([]ProgramDay, error) {
	eventDays, err := s.events.ListDays(ctx, ev.ID)
	if err != nil {
		return nil, err
	}
	slices.SortStableFunc(eventDays, func(a, b event.Day) int {
		return cmp.Or(compareTimes(a.StartDate, b.StartDate), strings.Compare(a.Name, b.Name), strings.Compare(a.ID.String(), b.ID.String()))
	})
	out := make([]ProgramDay, 0, len(eventDays))
	for _, day := range eventDays {
		sessions, err := s.events.ListSessions(ctx, day.ID)
		if err != nil {
			return nil, err
		}
		slices.SortStableFunc(sessions, func(a, b event.Session) int {
			return cmp.Or(compareTimes(a.StartTime, b.StartTime), cmp.Compare(a.OrderIndex, b.OrderIndex), strings.Compare(a.Title, b.Title))
		})
		pd := ProgramDay{Date: day.StartDate}
		for _, sess := range sessions {
			if sess.Cancelled {
				continue
			}
			if pd.Date == nil {
				pd.Date = sess.StartTime
			}
			pd.Sessions = append(pd.Sessions, ProgramSession{Start: sess.StartTime, End: sess.EndTime, Title: sess.Title, Speaker: sess.SpeakerName})
		}
		if len(pd.Sessions) > 0 {
			out = append(out, pd)
		}
	}
	return out, nil
}

// compareTimes orders by time, a missing one last.
func compareTimes(a, b *time.Time) int {
	switch {
	case a == nil && b == nil:
		return 0
	case a == nil:
		return 1
	case b == nil:
		return -1
	default:
		return a.Compare(*b)
	}
}

// band is the cover's band, cut from its page size and kept a while; nil
// when the cover cannot be read (the band is then its colour).
func (s *Service) band(ctx context.Context, cover uuid.UUID) []byte {
	if s.images == nil {
		return nil
	}
	now := s.now()
	if data, ok := s.bands.get(cover, now); ok {
		return data
	}
	data, _, ok := s.images.Image(ctx, cover, media.SizePage)
	if !ok {
		return nil
	}
	cut, ok := CoverBand(data)
	if !ok {
		return nil
	}
	s.bands.put(cover, cut, now)
	return cut
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
