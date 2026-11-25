package certificate

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/media"
	"github.com/skylab-kulubu/core-backend/internal/organizer"
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
	// PublicPageOrigin is the certificate page's address on the club's site
	// (CERTIFICATE_PUBLIC_PAGE_ORIGIN, read through PublicPageOrigin), e.g.
	// https://yildizskylab.com/sertifika. With it a certificate's pdfUrl is
	// {PublicPageOrigin}/{serial}.pdf, which the site streams from core
	// (docs/certificate-pdf-address.md); empty keeps core's own
	// {PublicAPIOrigin}/v1/certificates/verify/{serial}/pdf. Core's route keeps
	// answering either way, for the links already sent.
	PublicPageOrigin string
	LegacyImmediate  bool
	// Media checks each Media a template draft is about to link. Nil leaves
	// the Media's rules to the database's guards.
	Media media.Linker
	// Attendance reads an Event's attendance for the rule. Nil takes the
	// store's own query (PostgresStore) or, for a store without one, reads
	// the other stores (the memory counterpart).
	Attendance AttendanceStore
	// AutoFinalize plans the automatic attendance finalization. Nil takes
	// the store's (PostgresStore); a store without one leaves AutoFinalize
	// refused (ErrInvalid).
	AutoFinalize AutoFinalizeStore
	// Mails is the certificate mail queue (docs/certificate-mail.md). Nil
	// takes the store's (PostgresStore, MemoryStore). The queue is used
	// only with a Mailer that is a CertificateMailer; any other is sent to
	// directly, as before.
	Mails MailQueue
	// MailPDF sends each certificate's PDF with its queued mail
	// (CERTIFICATE_PDF_ATTACHMENT; docs/certificate-mail.md). It needs a
	// Mailer that is a CertificateAttachmentMailer; a PDF over SkyMail's
	// bound, or one that cannot be read, leaves the mail without it.
	MailPDF bool
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
	pageOrigin       string
	templates        TemplateStore
	jobs             JobStore
	artifacts        ArtifactStore
	assets           AssetReader
	privateArtifacts media.PrivateObjects
	legacyImmediate  bool
	media            media.Linker
	attendance       AttendanceStore
	autoFinalize     AutoFinalizeStore
	autoMetrics      autoFinalizeMetrics
	mails            MailQueue
	mailPDFOn        bool
	mailPDF          mailPDFMetrics
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
	pageOrigin := strings.TrimRight(strings.TrimSpace(opts.PublicPageOrigin), "/")
	if opts.Templates == nil {
		opts.Templates, _ = store.(TemplateStore)
	}
	if opts.Jobs == nil {
		opts.Jobs, _ = store.(JobStore)
	}
	if opts.Attendance == nil {
		opts.Attendance, _ = store.(AttendanceStore)
	}
	if opts.AutoFinalize == nil {
		opts.AutoFinalize, _ = store.(AutoFinalizeStore)
	}
	if opts.Mails == nil {
		opts.Mails, _ = store.(MailQueue)
	}
	if opts.Attendance == nil {
		opts.Attendance = memoryAttendance{events: events, tickets: tickets, certs: store, jobs: opts.Jobs, users: users}
	}
	return &service{
		store: store, tickets: tickets, events: events, users: users, authz: az,
		render: render, mail: mail, apiOrigin: apiOrigin, verifyOrigin: verifyOrigin, pageOrigin: pageOrigin,
		templates: opts.Templates, jobs: opts.Jobs, artifacts: opts.Artifacts,
		assets: opts.Assets, legacyImmediate: opts.LegacyImmediate, media: opts.Media, privateArtifacts: opts.PrivateArtifacts,
		attendance: opts.Attendance, autoFinalize: opts.AutoFinalize, mails: opts.Mails,
		mailPDFOn: opts.MailPDF,
	}
}

// eventResource is ev's decision of type t, its Co-organizers included
// (event.AuthzResource, docs/co-organizers.md).
func eventResource(ev event.Event, t authz.Type) authz.Resource {
	return event.AuthzResource(ev, t, time.Now())
}

func (s *service) canIssue(p authz.Principal, ev event.Event) bool {
	return s.authz.Allow(p, eventResource(ev, authz.TypeCertificate), authz.Issue)
}

func (s *service) canReadWorkspace(p authz.Principal, ev event.Event) bool {
	return s.authz.Allow(p, eventResource(ev, authz.TypeCertificate), authz.Read) ||
		s.authz.Allow(p, eventResource(ev, authz.TypeCertificateTemplate), authz.Read)
}

// certificateResource is the decision on a certificate c: its Event's, the
// Event's Co-organizers included; the Owner team it was issued under when
// the Event cannot be read.
func (s *service) certificateResource(ctx context.Context, c Certificate) authz.Resource {
	if s.events != nil {
		if ev, err := s.events.GetIncludingArchived(ctx, c.EventID); err == nil {
			return eventResource(ev, authz.TypeCertificate)
		}
	}
	return authz.Resource{Type: authz.TypeCertificate, OwnerTeam: c.OwnerTeam}
}

// eligible decides one Ticket by the Event's rule over the counted and
// required live Sessions (docs/certificate-rule.md). It reads the Event's
// attendance in a fixed number of queries; a caller deciding every Ticket
// reads it once with eventAttendance instead.
func (s *service) eligible(ctx context.Context, ev event.Event, t ticket.Ticket) (bool, error) {
	attendance, err := s.attendance.EventAttendance(ctx, ev.ID)
	if err != nil {
		return false, err
	}
	row, ok := attendance.ticketOf(t.ID)
	return ok && ticketEligible(ev, attendance, row), nil
}

// attendanceSnapshot is the Ticket's attendance as its certificate is
// issued, which the certificate keeps (docs/certificate-rule.md): a reissue
// takes a new one, and one issued below the rule by hand carries its real
// count.
func (s *service) attendanceSnapshot(ctx context.Context, eventID, ticketID uuid.UUID) (*AttendanceSnapshot, error) {
	attendance, err := s.attendance.EventAttendance(ctx, eventID)
	if err != nil {
		return nil, err
	}
	return attendance.snapshotOf(ticketID), nil
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

// pdfURL is where a certificate's PDF opens: on the club's site when the
// page origin is set, else on core (Options.PublicPageOrigin).
func (s *service) pdfURL(serial string) string {
	if s.pageOrigin != "" {
		return s.pageOrigin + "/" + serial + ".pdf"
	}
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
	attendance, err := s.attendanceSnapshot(ctx, ev.ID, t.ID)
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
	c := Certificate{EventID: ev.ID, TicketID: t.ID, OwnerID: t.OwnerID, Serial: serial, RecipientName: name, RecipientEmail: email, EventName: ev.Name, OwnerTeam: ev.OwnerTeam, VerifyURL: url, TemplateSource: "legacy", Attendance: attendance, CoOrganizerTeams: coOrganizerSnapshot(ev)}
	created, err := s.store.Create(ctx, c, pdf)
	if err != nil {
		return Certificate{}, err
	}
	if err := s.queueMail(ctx, created); err != nil {
		// Not queued: sent inline instead, as before the queue.
		log.Printf("ERROR certificate %s: mail not queued, sent inline (%s)", created.ID, databaseErrorClass(err))
		s.sendDirect(ctx, created)
	} else {
		s.sendMail(ctx, created)
	}
	return created, nil
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
	if ev.Status != event.StatusPublished {
		return nil, nil
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
	ev, err := s.visibleEvent(ctx, p, eventID)
	if err != nil {
		return nil, err
	}
	if !s.canIssue(p, ev) {
		return nil, ErrForbidden
	}
	if err := issuable(ev); err != nil {
		return nil, err
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
	ev, err := s.visibleEvent(ctx, p, eventID)
	if err != nil {
		return Certificate{}, err
	}
	if !s.canIssue(p, ev) {
		return Certificate{}, ErrForbidden
	}
	if err := issuable(ev); err != nil {
		return Certificate{}, err
	}
	t, err := s.tickets.Get(ctx, ticketID)
	if err != nil {
		return Certificate{}, mapTicketError(err)
	}
	if t.EventID != eventID {
		return Certificate{}, ErrInvalid
	}
	if !t.Active() {
		return Certificate{}, ErrNotFound
	}
	return s.materializeLegacy(ctx, ev, t)
}

func (s *service) Revoke(ctx context.Context, p authz.Principal, serial string) error {
	c, _, err := s.store.GetBySerial(ctx, serial)
	if err != nil {
		return err
	}
	if !s.authz.Allow(p, s.certificateResource(ctx, c), authz.Revoke) {
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
	return PublicCertificate{Serial: c.Serial, RecipientName: c.RecipientName, EventName: c.EventName, OwnerTeam: c.OwnerTeam, OrganizerName: organizer.Name(c.OwnerTeam), CoOrganizerNames: coOrganizerNames(c), Status: status, VerifyURL: c.VerifyURL, PDFURL: pdfURL, IssuedAt: c.IssuedAt, RevokedAt: c.RevokedAt, Attendance: publicAttendance(c.Attendance)}, nil
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
			ID: c.ID, Serial: c.Serial, Event: MineCertificateEvent{ID: c.EventID, Name: c.EventName, OwnerTeam: c.OwnerTeam, OrganizerName: organizer.Name(c.OwnerTeam)},
			Status: status, IssuedAt: c.IssuedAt, RevokedAt: c.RevokedAt, PDFURL: pdfURL, VerifyURL: c.VerifyURL,
			Share: ShareInfo{Title: c.EventName + " sertifikası", Text: c.RecipientName + " · " + c.EventName, URL: c.VerifyURL},
		})
	}
	return out, nil
}

func (s *service) ListByEvent(ctx context.Context, p authz.Principal, eventID uuid.UUID) ([]Certificate, error) {
	ev, err := s.visibleEvent(ctx, p, eventID)
	if err != nil {
		return nil, err
	}
	if !s.authz.Allow(p, eventResource(ev, authz.TypeCertificate), authz.Read) {
		return nil, ErrForbidden
	}
	certs, err := s.store.ListByEvent(ctx, eventID)
	if err != nil {
		return nil, err
	}
	states, err := s.mailStates(ctx, eventID)
	if err != nil {
		return nil, err
	}
	for i := range certs {
		if state, ok := states[certs[i].Serial]; ok {
			certs[i].Mail = &state
		}
	}
	return certs, nil
}

// visibleEvent is the current Event id names, ErrNotFound when p may not see
// it: a draft's certificates do not exist for who may not see the draft
// (event.VisibleTo, ADR 0066).
func (s *service) visibleEvent(ctx context.Context, p authz.Principal, id uuid.UUID) (event.Event, error) {
	ev, err := s.events.Get(ctx, id)
	if err != nil {
		return event.Event{}, mapEventError(err)
	}
	if !event.VisibleTo(s.authz, &p, ev) {
		return event.Event{}, ErrNotFound
	}
	return ev, nil
}

// issuable refuses a certificate on an Event that is not published (ADR
// 0066): a draft and a cancelled Event issue none.
func issuable(ev event.Event) error {
	if err := ev.StatusError(); err != nil {
		return fmt.Errorf("%w: %w", ErrConflict, err)
	}
	return nil
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
