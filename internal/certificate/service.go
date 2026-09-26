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
	"github.com/skylab-kulubu/core-backend/internal/media"
	"github.com/skylab-kulubu/core-backend/internal/qr"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

type Options struct {
	PublicAPIOrigin string
	VerifyOrigin    string
	Templates       TemplateStore
	Jobs            JobStore
	Artifacts       ArtifactStore
	Assets          AssetReader
	// PrivateArtifacts keeps the copies of private assets a published
	// version takes. Nil while private Media is off.
	PrivateArtifacts media.PrivateObjects
	LegacyImmediate  bool
	// Media checks each Media a template draft is about to link. Nil leaves
	// the Media's rules to the database's guards.
	Media media.Linker
}

type service struct {
	store            Store
	tickets          ticket.Store
	events           event.Store
	users            user.Store
	authz            authz.Authorizer
	render           Renderer
	mail             Mailer
	apiOrigin        string
	verifyOrigin     string
	templates        TemplateStore
	jobs             JobStore
	artifacts        ArtifactStore
	assets           AssetReader
	privateArtifacts media.PrivateObjects
	legacyImmediate  bool
	media            media.Linker
}

// NewService preserves the original synchronous contract for existing embedders.
// Production uses NewServiceWithOptions and the durable issuance worker.
func NewService(store Store, tickets ticket.Store, events event.Store, users user.Store, az authz.Authorizer, render Renderer, mail Mailer, publicOrigin string) Service {
	return NewServiceWithOptions(store, tickets, events, users, az, render, mail, Options{
		PublicAPIOrigin: publicOrigin,
		VerifyOrigin:    strings.TrimRight(publicOrigin, "/") + "/v1/certificates/verify",
		LegacyImmediate: true,
	})
}

func NewServiceWithOptions(store Store, tickets ticket.Store, events event.Store, users user.Store, az authz.Authorizer, render Renderer, mail Mailer, opts Options) Service {
	apiOrigin := strings.TrimRight(opts.PublicAPIOrigin, "/")
	if apiOrigin == "" {
		apiOrigin = strings.TrimRight(os.Getenv("PUBLIC_API_ORIGIN"), "/")
	}
	if apiOrigin == "" {
		apiOrigin = "https://api.yildizskylab.com"
	}
	verifyOrigin := strings.TrimRight(opts.VerifyOrigin, "/")
	if verifyOrigin == "" {
		verifyOrigin = strings.TrimRight(os.Getenv("PUBLIC_VERIFY_ORIGIN"), "/")
	}
	if verifyOrigin == "" {
		verifyOrigin = "https://skyl.app/c"
	}
	if opts.Templates == nil {
		opts.Templates, _ = store.(TemplateStore)
	}
	if opts.Jobs == nil {
		opts.Jobs, _ = store.(JobStore)
	}
	return &service{
		store: store, tickets: tickets, events: events, users: users, authz: az,
		render: render, mail: mail, apiOrigin: apiOrigin, verifyOrigin: verifyOrigin,
		templates: opts.Templates, jobs: opts.Jobs, artifacts: opts.Artifacts,
		assets: opts.Assets, legacyImmediate: opts.LegacyImmediate, media: opts.Media, privateArtifacts: opts.PrivateArtifacts,
	}
}

func (s *service) canIssue(p authz.Principal, ownerTeam string) bool {
	return s.authz.Allow(p, authz.Resource{Type: authz.TypeCertificate, OwnerTeam: ownerTeam}, authz.Issue)
}

func (s *service) canReadWorkspace(p authz.Principal, ownerTeam string) bool {
	return s.authz.Allow(p, authz.Resource{Type: authz.TypeCertificate, OwnerTeam: ownerTeam}, authz.Read) ||
		s.authz.Allow(p, authz.Resource{Type: authz.TypeCertificateTemplate, OwnerTeam: ownerTeam}, authz.Read)
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
	for _, checkIn := range t.CheckIns {
		if _, ok := live[checkIn.SessionID]; ok {
			checkIns++
		}
	}
	return scheduled, checkIns, nil
}

func (s *service) eligible(ctx context.Context, ev event.Event, t ticket.Ticket) (bool, error) {
	scheduled, checkIns, err := s.scheduledAndCheckIns(ctx, t)
	if err != nil {
		return false, err
	}
	ratio := 0.0
	if ev.AttendanceRatio != nil {
		ratio = *ev.AttendanceRatio
	}
	return Eligible(ev.AttendanceRule, ratio, checkIns, scheduled), nil
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
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	return strings.ToUpper(hex.EncodeToString(random[:])), nil
}

func (s *service) verifyURL(serial string) string {
	return s.verifyOrigin + "/" + serial
}

func (s *service) pdfURL(serial string) string {
	return s.apiOrigin + "/v1/certificates/verify/" + serial + "/pdf"
}

func (s *service) materializeLegacy(ctx context.Context, ev event.Event, t ticket.Ticket) (Certificate, error) {
	if _, err := s.store.GetActive(ctx, ev.ID, t.ID); err == nil {
		return Certificate{}, ErrConflict
	} else if !errors.Is(err, ErrNotFound) {
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
	if s.render == nil {
		return Certificate{}, ErrInvalid
	}
	pdf, err := s.render.PDF(ctx, HTML(ev.OwnerTeam, name, ev.Name, url, qrPNG))
	if err != nil {
		return Certificate{}, err
	}
	c := Certificate{EventID: ev.ID, TicketID: t.ID, OwnerID: t.OwnerID, Serial: serial, RecipientName: name, RecipientEmail: email, EventName: ev.Name, OwnerTeam: ev.OwnerTeam, VerifyURL: url, TemplateSource: "legacy"}
	created, err := s.store.Create(ctx, c, pdf)
	if err != nil {
		return Certificate{}, err
	}
	s.sendMail(ctx, created)
	return created, nil
}

func (s *service) sendMail(ctx context.Context, c Certificate) {
	if s.mail == nil {
		return
	}
	first := c.RecipientName
	if parts := strings.Fields(c.RecipientName); len(parts) > 0 {
		first = parts[0]
	}
	s.mail.Certificate(ctx, c.RecipientEmail, c.RecipientName, map[string]string{
		"FirstName": first, "EventName": c.EventName, "VerifyURL": c.VerifyURL,
		"Serial": c.Serial, "OwnerTeam": c.OwnerTeam,
	})
}

func (s *service) RecomputeTicket(ctx context.Context, ticketID uuid.UUID) (*Certificate, error) {
	if !s.legacyImmediate {
		return nil, nil
	}
	t, err := s.tickets.Get(ctx, ticketID)
	if err != nil {
		return nil, mapTicketError(err)
	}
	ev, err := s.events.Get(ctx, t.EventID)
	if err != nil {
		return nil, mapEventError(err)
	}
	ok, err := s.eligible(ctx, ev, t)
	if err != nil || !ok {
		return nil, err
	}
	if _, err := s.store.GetActive(ctx, ev.ID, t.ID); err == nil {
		return nil, nil
	} else if !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	created, err := s.materializeLegacy(ctx, ev, t)
	if err != nil {
		return nil, err
	}
	return &created, nil
}

func (s *service) RecomputeEvent(ctx context.Context, p authz.Principal, eventID uuid.UUID) ([]Certificate, error) {
	if !s.legacyImmediate {
		_, err := s.Finalize(ctx, p, eventID)
		return []Certificate{}, err
	}
	ev, err := s.events.Get(ctx, eventID)
	if err != nil {
		return nil, mapEventError(err)
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
	if !s.legacyImmediate {
		return Certificate{}, ErrInvalid
	}
	ev, err := s.events.Get(ctx, eventID)
	if err != nil {
		return Certificate{}, mapEventError(err)
	}
	if !s.canIssue(p, ev.OwnerTeam) {
		return Certificate{}, ErrForbidden
	}
	t, err := s.tickets.Get(ctx, ticketID)
	if err != nil {
		return Certificate{}, mapTicketError(err)
	}
	if t.EventID != eventID {
		return Certificate{}, ErrInvalid
	}
	return s.materializeLegacy(ctx, ev, t)
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

func (s *service) Verify(ctx context.Context, serial string) (PublicCertificate, error) {
	c, _, err := s.store.GetBySerial(ctx, serial)
	if err != nil {
		return PublicCertificate{}, err
	}
	status := "valid"
	pdfURL := s.pdfURL(c.Serial)
	if c.RevokedAt != nil {
		status = "revoked"
		pdfURL = ""
	}
	return PublicCertificate{Serial: c.Serial, RecipientName: c.RecipientName, EventName: c.EventName, OwnerTeam: c.OwnerTeam, Status: status, VerifyURL: c.VerifyURL, PDFURL: pdfURL, IssuedAt: c.IssuedAt, RevokedAt: c.RevokedAt}, nil
}

func (s *service) PDF(ctx context.Context, serial string) ([]byte, error) {
	c, pdf, err := s.store.GetBySerial(ctx, serial)
	if err != nil {
		return nil, err
	}
	if c.RevokedAt != nil {
		return nil, ErrNotFound
	}
	if c.PDFKey != "" {
		if s.artifacts == nil {
			return nil, ErrInvalid
		}
		return s.artifacts.Read(ctx, c.PDFKey)
	}
	return pdf, nil
}

func (s *service) Mine(ctx context.Context, p authz.Principal) ([]MineCertificate, error) {
	id, err := uuid.Parse(p.ID)
	if err != nil {
		return nil, ErrInvalid
	}
	certs, err := s.store.ListByOwner(ctx, id)
	if err != nil {
		return nil, err
	}
	out := make([]MineCertificate, 0, len(certs))
	for _, c := range certs {
		status := "valid"
		pdfURL := s.pdfURL(c.Serial)
		if c.RevokedAt != nil {
			status = "revoked"
			pdfURL = ""
		}
		out = append(out, MineCertificate{
			ID: c.ID, Serial: c.Serial, Event: MineCertificateEvent{ID: c.EventID, Name: c.EventName, OwnerTeam: c.OwnerTeam},
			Status: status, IssuedAt: c.IssuedAt, RevokedAt: c.RevokedAt, PDFURL: pdfURL, VerifyURL: c.VerifyURL,
			Share: ShareInfo{Title: c.EventName + " sertifikası", Text: c.RecipientName + " · " + c.EventName, URL: c.VerifyURL},
		})
	}
	return out, nil
}

func (s *service) ListByEvent(ctx context.Context, p authz.Principal, eventID uuid.UUID) ([]Certificate, error) {
	ev, err := s.events.Get(ctx, eventID)
	if err != nil {
		return nil, mapEventError(err)
	}
	if !s.authz.Allow(p, authz.Resource{Type: authz.TypeCertificate, OwnerTeam: ev.OwnerTeam}, authz.Read) {
		return nil, ErrForbidden
	}
	return s.store.ListByEvent(ctx, eventID)
}

func mapEventError(err error) error {
	if errors.Is(err, event.ErrNotFound) {
		return ErrNotFound
	}
	return err
}

func mapTicketError(err error) error {
	if errors.Is(err, ticket.ErrNotFound) {
		return ErrNotFound
	}
	return err
}
