// Package ticketnotify tells a new Ticket's holder that the Ticket is there
// (iletişim-servisi 06, ADR 0070; docs/ticket-created-notification.md):
// core says what happened and to whom, SkyNotify picks the channels (inbox and
// e-mail) and the text. Member apply, Apply-for-other and Guest apply queue
// the Ticket in the statement that writes it (ticket_notifications); this
// worker reads the holder when it sends, posts the Notification under a key
// of the Ticket, so a retry never tells anyone twice, and removes the row.
package ticketnotify

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/mail"
	"github.com/skylab-kulubu/core-backend/internal/organizer"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

const (
	// RetryFirst is the wait before a Notification that failed is tried
	// again; it doubles with each failure in a row, up to retryMax.
	RetryFirst = 30 * time.Second
	retryMax   = 30 * time.Minute
	// GiveUpAfter is how long a Notification is tried, and sent at all: a
	// ticket mail a day late is no longer news.
	GiveUpAfter = 24 * time.Hour
	// lease is how long a pass holds the rows it took.
	lease = 2 * time.Minute
	// PDFTimeout bounds drawing one Ticket's PDF for the mail
	// (TICKET_PDF_ATTACHMENT): Gotenberg slower than this, or failing, and
	// the mail goes without it.
	PDFTimeout = 2 * time.Minute
	// pdfLease is the lease while the PDF goes along: a row is started only
	// while a whole PDFTimeout and sendMargin are left of it, so no other
	// core takes a row this one is still sending.
	pdfLease     = 5 * time.Minute
	sendMargin   = time.Minute
	pollInterval = 10 * time.Second
	// batch bounds one claim.
	batch        = 50
	writeTimeout = 5 * time.Second
	timeZone     = "Europe/Istanbul"
	dateLayout   = "02.01.2006 15:04"
	// keyVersion is the version in the Idempotency-Key: a later change of
	// what one Ticket's Notification says takes a new one.
	keyVersion = "v1"
)

// Queued is one Ticket waiting for its Notification.
type Queued struct {
	TicketID  uuid.UUID
	CreatedAt time.Time
	Attempts  int
	// ClaimedAt is the claim that returned the row: Complete and Retry write
	// only while the row still carries it.
	ClaimedAt time.Time
	// Resend is which time the panel sent the Ticket's mail again
	// (docs/ticket-resend.md): 0 is its first mail.
	Resend int
	// Kind is the mail (docs/event-capacity.md): KindCreated ("Biletin
	// hazır", every row before the waiting list, and the empty kind),
	// KindWaitlisted, KindPromoted or KindCancelled.
	Kind string
}

// The kinds of a queued row (ticket_notifications.kind).
const (
	KindCreated    = "created"
	KindWaitlisted = "waitlisted"
	KindPromoted   = "promoted"
	// KindCancelled is "Biletin iptal edildi" (docs/ticket-status-mail.md).
	KindCancelled = "cancelled"
)

// Queue keeps the Tickets waiting (PostgresQueue: ticket_notifications).
type Queue interface {
	// Claim takes up to limit due rows, oldest first, and holds them for
	// lease.
	Claim(ctx context.Context, now time.Time, lease time.Duration, limit int) ([]Queued, error)
	// Complete removes a claimed row whose Notification is done with.
	Complete(ctx context.Context, claimed Queued) error
	// Retry counts a failure, makes the row due at at and releases it.
	Retry(ctx context.Context, claimed Queued, at time.Time) error
}

// Mailer posts the Notification (mail.SkyNotify).
type Mailer interface {
	TicketCreated(ctx context.Context, idempotencyKey string, holder mail.TicketHolder, vars map[string]any, source mail.NotificationSource, expiresAt *time.Time, attachments []mail.Attachment) error
}

// StatusMailer posts the waiting list's and the cancellation's mails
// (mail.SkyNotify): typ is mail.WaitlistJoinedType, mail.WaitlistPromotedType
// or mail.TicketCancelledType. A Mailer that is not one drops those rows.
type StatusMailer interface {
	TicketStatus(ctx context.Context, typ, idempotencyKey string, holder mail.TicketHolder, vars map[string]any, source mail.NotificationSource, expiresAt *time.Time, attachments []mail.Attachment) error
}

// PDFs draws the PDF ticket an access key opens and names it
// (ticketpage.Service.PDF; docs/ticket-page.md).
type PDFs interface {
	PDF(ctx context.Context, key string) (filename string, pdf []byte, err error)
}

// Tickets, Events and Users are what the worker reads when it sends.
type Tickets interface {
	Get(ctx context.Context, id uuid.UUID) (ticket.Ticket, error)
}

type Events interface {
	GetIncludingArchived(ctx context.Context, id uuid.UUID) (event.Event, error)
}

type Users interface {
	Get(ctx context.Context, id uuid.UUID) (user.User, error)
}

// Config is what a Service takes.
type Config struct {
	Queue   Queue
	Tickets Tickets
	Events  Events
	Users   Users
	Mail    Mailer
	// Page is the ticket page's settings (docs/ticket-page.md): while its
	// URL is set, the Notification carries the Ticket's page, QR image and
	// code. APIOrigin is core's public origin, where the QR image is.
	Page      ticket.PageConfig
	APIOrigin string
	// PDF, when set (TICKET_PDF_ATTACHMENT=on), draws the Ticket's PDF just
	// before each send, and it goes with the e-mail as a Mail attachment.
	// Nil sends no file, as before.
	PDF PDFs
	// Now is the clock; nil is time.Now.
	Now func() time.Time
	// Logger hears counts and failures, never a person; nil is log.Default.
	Logger *log.Logger
}

// Service sends the queued Notifications (Run).
type Service struct {
	config   Config
	now      func() time.Time
	logger   *log.Logger
	location *time.Location
}

// NewService checks config.
func NewService(config Config) (*Service, error) {
	if config.Queue == nil || config.Tickets == nil || config.Events == nil || config.Users == nil || config.Mail == nil {
		return nil, errors.New("ticketnotify: a queue, the Tickets, Events and users and a mailer are needed")
	}
	location, err := time.LoadLocation(timeZone)
	if err != nil {
		return nil, fmt.Errorf("ticketnotify: %w", err)
	}
	now := config.Now
	if now == nil {
		now = time.Now
	}
	logger := config.Logger
	if logger == nil {
		logger = log.Default()
	}
	return &Service{config: config, now: now, logger: logger, location: location}, nil
}

// Report counts one pass.
type Report struct {
	// Sent: SkyNotify took the Notification. Dropped: nothing to tell any
	// more (the Ticket, its Event or its holder is gone, the Event is not
	// published, or is over). Rejected: SkyNotify refused the request itself.
	// Expired: tried for GiveUpAfter. Retried: tried again later.
	// Uncertain: a guest's mail went out and no answer came; it is not
	// tried again, so the guest is not mailed twice.
	Sent, Dropped, Rejected, Expired, Retried, Uncertain int
	// PDFSkipped: while the PDF goes along, a mail sent (or tried) without
	// it: Gotenberg failed or took longer than PDFTimeout, the PDF was over
	// SkyNotify's 512 KB, or SkyNotify refused the body with it.
	PDFSkipped int
}

func (r Report) any() bool {
	return r.Sent+r.Dropped+r.Rejected+r.Expired+r.Retried+r.Uncertain > 0
}

// IdempotencyKey is the key of a Ticket's Notification: the type, the
// Ticket and the version, the same on every try.
func IdempotencyKey(ticketID uuid.UUID) string {
	return "core:" + mail.TicketCreatedType + ":" + ticketID.String() + ":" + keyVersion
}

// ResendKey is the key of the n-th time the panel sent a Ticket's mail
// again: …:r<n>, its own, so SkyNotify sends it rather than answering the
// first mail's replay (docs/ticket-resend.md). n 0 is the first mail's.
func ResendKey(ticketID uuid.UUID, n int) string {
	if n <= 0 {
		return IdempotencyKey(ticketID)
	}
	return "core:" + mail.TicketCreatedType + ":" + ticketID.String() + ":r" + strconv.Itoa(n)
}

// StatusKey is the key of a waiting list mail of typ: the Ticket and its QR
// version, which a reopening raises, so each time on the list is told once.
func StatusKey(typ string, ticketID uuid.UUID, qrVersion int) string {
	return "core:" + typ + ":" + ticketID.String() + ":q" + strconv.Itoa(qrVersion)
}

// Pass sends the due Notifications of one claim.
func (s *Service) Pass(ctx context.Context) (Report, error) {
	var report Report
	held := lease
	if s.config.PDF != nil {
		held = pdfLease
	}
	claimedAt := s.now().UTC()
	claimed, err := s.config.Queue.Claim(ctx, claimedAt, held, batch)
	if err != nil {
		return report, err
	}
	// pdf is this pass's PDF switch: once Gotenberg fails or is too slow,
	// the rest of the pass sends without the file (the next pass tries it
	// again), so a Gotenberg down does not hold every mail for PDFTimeout.
	pdf := s.config.PDF != nil
	for _, q := range claimed {
		if s.config.PDF != nil && s.now().Sub(claimedAt) > held-PDFTimeout-sendMargin {
			// Too little of the lease left to draw a PDF and send: the rows
			// not yet started are taken again when it runs out, by this core
			// or another, under the same key.
			break
		}
		stale := s.now().Sub(q.CreatedAt) > GiveUpAfter
		var (
			outcome outcome
			err     error
		)
		if !stale {
			outcome, err = s.deliver(ctx, q, &pdf, &report)
		}
		writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), writeTimeout)
		switch {
		case stale:
			// A day late is no longer news, whether it failed until now or
			// waited while the worker was off.
			report.Expired++
			err = s.config.Queue.Complete(writeCtx, q)
		case err != nil && ctx.Err() != nil:
			// Core is stopping: the row is taken again once its lease runs
			// out, under the same key.
			cancel()
			return report, ctx.Err()
		case err != nil:
			report.Retried++
			s.logger.Printf("ticket notification: failed and is tried again: %v", err)
			err = s.config.Queue.Retry(writeCtx, q, s.now().UTC().Add(backoff(q.Attempts+1)))
		default:
			switch outcome {
			case sent:
				report.Sent++
			case dropped:
				report.Dropped++
			case rejected:
				report.Rejected++
			case uncertain:
				report.Uncertain++
			}
			err = s.config.Queue.Complete(writeCtx, q)
		}
		cancel()
		if err != nil {
			return report, err
		}
	}
	return report, nil
}

type outcome int

const (
	sent outcome = iota
	dropped
	rejected
	uncertain
)

// deliver posts q's Notification; an error when a later try may pass. pdf
// is the pass's PDF switch (Pass); a mail sent without the PDF while it
// should have gone along is counted in report.PDFSkipped.
func (s *Service) deliver(ctx context.Context, q Queued, pdf *bool, report *Report) (outcome, error) {
	t, err := s.config.Tickets.Get(ctx, q.TicketID)
	if errors.Is(err, ticket.ErrNotFound) {
		return dropped, nil
	}
	if err != nil {
		return 0, err
	}
	// A Ticket cancelled (or waitlisted) since it was queued is not ready:
	// no mail (docs/ticket-cancellation.md). "Bekleme listesindesin" goes
	// only while the Ticket still waits (docs/event-capacity.md).
	typ := mail.TicketCreatedType
	switch q.Kind {
	case KindCancelled:
		// "Biletin iptal edildi" goes only while the Ticket is still
		// cancelled: restored or registered again since, it is not news.
		typ = mail.TicketCancelledType
		if t.StatusOf() != ticket.StatusCancelled {
			return dropped, nil
		}
	case KindWaitlisted:
		typ = mail.WaitlistJoinedType
		if t.StatusOf() != ticket.StatusWaitlisted {
			return dropped, nil
		}
	case KindPromoted:
		typ = mail.WaitlistPromotedType
		fallthrough
	default:
		if !t.Active() {
			return dropped, nil
		}
	}
	status, _ := s.config.Mail.(StatusMailer)
	if typ != mail.TicketCreatedType && status == nil {
		return dropped, nil
	}
	ev, err := s.config.Events.GetIncludingArchived(ctx, t.EventID)
	if errors.Is(err, event.ErrNotFound) {
		return dropped, nil
	}
	if err != nil {
		return 0, err
	}
	// Only a published Event that is not over is told of: a draft's
	// Tickets (speakers, the team, added from the panel) and a cancelled
	// or archived Event's are not news to anyone. Over is the Event's end
	// as everywhere else (event.EndedAt: its end date, else a day after
	// its start), so a past Event without an end date is over too.
	if ev.ArchivedAt != nil || ev.Status != event.StatusPublished || ev.EndedAt(s.now()) {
		return dropped, nil
	}
	holder, ok, err := s.holder(ctx, t)
	if err != nil {
		return 0, err
	}
	if !ok {
		return dropped, nil
	}
	var attachments []mail.Attachment
	// The waiting list's and the cancellation's mails carry no ticket: no
	// PDF.
	if s.config.PDF != nil && q.Kind != KindWaitlisted && q.Kind != KindCancelled {
		attachments, err = s.attachment(ctx, t, ev, pdf)
		if err != nil {
			return 0, err
		}
		if attachments == nil {
			report.PDFSkipped++
		}
	}
	key, vars, source := ResendKey(t.ID, q.Resend), s.vars(ev, t), mail.NotificationSource{Kind: "ticket", Ref: t.ID.String()}
	send := func(attachments []mail.Attachment) error {
		return s.config.Mail.TicketCreated(ctx, key, holder, vars, source, ev.EndDate, attachments)
	}
	if typ != mail.TicketCreatedType {
		key = StatusKey(typ, t.ID, t.QRVersion)
		switch q.Kind {
		case KindWaitlisted:
			vars = waitlistedVars(vars)
		case KindCancelled:
			vars = cancelledVars(vars, t)
		}
		send = func(attachments []mail.Attachment) error {
			return status.TicketStatus(ctx, typ, key, holder, vars, source, ev.EndDate, attachments)
		}
	}
	err = send(attachments)
	var refused *mail.SendError
	if attachments != nil && errors.As(err, &refused) && refused.Permanent() {
		// SkyNotify refused the body with the file (400 attachment.*, say):
		// the link, the QR and the code are in the mail, so it goes without.
		// A refused request leaves no key behind: the same key is free.
		s.logger.Printf("ticket notification: SkyNotify refused the mail with the PDF (%d %s); sent without it", refused.Status, refused.Code)
		report.PDFSkipped++
		err = send(nil)
	}
	switch {
	case errors.Is(err, mail.ErrMaybeDelivered):
		return uncertain, nil
	case errors.As(err, &refused) && refused.Permanent():
		// SkyNotify refuses this body itself (400, 422): the same body would be
		// refused again.
		return rejected, nil
	case err != nil:
		return 0, err
	}
	return sent, nil
}

// attachment is t's PDF as the mail's attachment, drawn now (a retry draws
// it again; SkyNotify keeps the first try's file under the same key), or nil
// when the mail goes without it: Gotenberg failed or took longer than
// PDFTimeout (and then *pdf turns off for the rest of the pass), or the
// file is over SkyNotify's 512 KB. The log names the Event, never the holder.
// An error only when ctx ends: core is stopping.
func (s *Service) attachment(ctx context.Context, t ticket.Ticket, ev event.Event, pdf *bool) ([]mail.Attachment, error) {
	if !*pdf {
		return nil, nil
	}
	drawCtx, cancel := context.WithTimeout(ctx, PDFTimeout)
	defer cancel()
	name, data, err := s.config.PDF.PDF(drawCtx, t.AccessKey)
	switch {
	case ctx.Err() != nil:
		return nil, ctx.Err()
	case err != nil:
		*pdf = false
		s.logger.Printf("ticket notification: no PDF for event %s, the rest of this pass goes without it: %v", ev.ID, err)
		return nil, nil
	case len(data) > mail.MaxAttachmentBytes:
		s.logger.Printf("ticket notification: the PDF of event %s is %d KB, over SkyNotify's %d KB; the mail goes without it", ev.ID, (len(data)+1023)/1024, mail.MaxAttachmentBytes/1024)
		return nil, nil
	}
	return []mail.Attachment{mail.PDFAttachment(name, data)}, nil
}

// holder is whom t's Notification goes to: a registered holder's subject
// while their account is active, a guest's address. Not ok when nobody may
// be written to.
func (s *Service) holder(ctx context.Context, t ticket.Ticket) (mail.TicketHolder, bool, error) {
	if t.TicketType == ticket.Guest {
		email := strings.ToLower(strings.TrimSpace(t.GuestEmail))
		if email == "" {
			return mail.TicketHolder{}, false, nil
		}
		return mail.TicketHolder{Email: email, FullName: joinName(t.GuestFirstName, t.GuestLastName)}, true, nil
	}
	if t.OwnerID == nil || *t.OwnerID == user.DeletedSubject {
		return mail.TicketHolder{}, false, nil
	}
	u, err := s.config.Users.Get(ctx, *t.OwnerID)
	switch {
	case errors.Is(err, user.ErrNotFound), errors.Is(err, user.ErrAccountBlocked):
		return mail.TicketHolder{}, false, nil
	case err != nil:
		return mail.TicketHolder{}, false, err
	case u.AccountState != user.AccountActive:
		return mail.TicketHolder{}, false, nil
	}
	return mail.TicketHolder{Sub: *t.OwnerID}, true, nil
}

// vars are the type's variables (SkyNotify emails/event-ticket-created.tsx):
// the Event's, the same for any holder. EventId fills the inbox item's link.
// Every key is always there, EventDate empty without a start: SkyNotify
// renders the inbox text with missingkey=error.
//
// While TICKET_PAGE_URL is set, the Ticket's own three join them (ADR 0082,
// docs/ticket-created-notification.md): TicketUrl, its page; TicketQrUrl,
// its QR as a remote image; TicketCode, its code as text. A member and a
// guest get the same. EventUrl is never sent: the site has no Event page.
// TicketNote and TicketNoteParts are the organizer's announcement
// (docs/ticket-note.md); event.waitlist-promoted gets them too, the
// waiting list's and the cancellation's mails do not.
func (s *Service) vars(ev event.Event, t ticket.Ticket) map[string]any {
	vars := map[string]any{
		"EventId":   ev.ID.String(),
		"EventName": ev.Name,
		"EventDate": "",
		"Venue":     ev.Location,
		// The template's "Düzenleyen" row: the organizer as the club shows
		// it (organizer.Name), the club's name for YK, DK, ADMIN or none.
		"OwnerTeam": organizer.Name(ev.OwnerTeam),
		// The Co-organizer teams shown outside the panel, for a template
		// that names them (docs/co-organizers.md); empty for none.
		"CoOrganizers": organizer.CoNames(event.PublicCoOrganizerTeams(ev)),
	}
	if ev.StartDate != nil {
		vars["EventDate"] = ev.StartDate.In(s.location).Format(dateLayout)
	}
	if link := s.config.Page.Link(t.AccessKey); link != "" {
		vars["TicketUrl"] = link
		vars["TicketCode"] = t.Code
		vars["TicketQrUrl"] = ""
		if origin := strings.TrimRight(s.config.APIOrigin, "/"); origin != "" {
			vars["TicketQrUrl"] = origin + "/v1/public/tickets/" + t.AccessKey + "/qr.png"
		}
	}
	// The organizer's announcement (docs/ticket-note.md): always there, ""
	// and [] for none; the parts link its https addresses.
	vars["TicketNote"], vars["TicketNoteParts"] = event.TicketNoteParts(ev)
	return vars
}

// waitlistedVars are event.waitlist-joined's variables (skynotify-frontend
// #179): the Event's and TicketUrl, the ticket page where the holder may
// leave the list (empty without TICKET_PAGE_URL). No QR and no code: a
// waitlisted Ticket opens no door (its QR answers 410).
func waitlistedVars(vars map[string]any) map[string]any {
	out := make(map[string]any, 6)
	for _, name := range []string{"EventId", "EventName", "EventDate", "OwnerTeam", "Venue", "TicketUrl"} {
		out[name] = vars[name]
	}
	return out
}

// cancelledVars are event.ticket-cancelled's variables (skynotify-frontend
// #179): the Event's, CancelledBy (holder or operator) and Reason, the
// operator's reason (empty for the holder, or when none was given), all of
// them always. No link, QR or code: a cancelled Ticket opens nothing.
func cancelledVars(vars map[string]any, t ticket.Ticket) map[string]any {
	out := make(map[string]any, 6)
	for _, name := range []string{"EventId", "EventName", "EventDate", "OwnerTeam"} {
		out[name] = vars[name]
	}
	out["CancelledBy"] = string(t.CancelledBy)
	out["Reason"] = ""
	if t.CancelledBy == ticket.CancelledByOperator {
		out["Reason"] = t.CancelReason
	}
	return out
}

func joinName(first, last string) string {
	return strings.TrimSpace(strings.TrimSpace(first) + " " + strings.TrimSpace(last))
}

func backoff(failures int) time.Duration {
	wait := RetryFirst
	for i := 1; i < failures && wait < retryMax; i++ {
		wait *= 2
	}
	return min(wait, retryMax)
}

// Run sends the queued Notifications until ctx is cancelled; the channel
// closes once it has stopped.
func (s *Service) Run(ctx context.Context, logf func(string, ...any)) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(pollInterval)
		defer ticker.Stop()
		for {
			report, err := s.Pass(ctx)
			switch {
			case err != nil && ctx.Err() == nil:
				logf("ticket notification: queue: %v", err)
			case report.any():
				logf("ticket notification: %d sent, %d dropped, %d refused by SkyNotify, %d given up, %d tried again later, %d guest mails unanswered and not tried again, %d pdf_skipped",
					report.Sent, report.Dropped, report.Rejected, report.Expired, report.Retried, report.Uncertain, report.PDFSkipped)
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	return done
}
