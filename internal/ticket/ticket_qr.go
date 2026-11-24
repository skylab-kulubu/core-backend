package ticket

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/ticketqr"
)

// The Ticket QR at the door (ADR 0082; docs/ticket-qr.md).
var (
	// ErrTicketQRInvalid is a code that is not a Ticket QR core signed.
	ErrTicketQRInvalid = errors.New("ticket: ticket qr invalid")
	// ErrTicketQRExpired is a genuine Ticket QR past its Event: a day after
	// the Event ends, by its dates now.
	ErrTicketQRExpired = errors.New("ticket: ticket qr expired")
	// ErrTicketQRSuperseded is a genuine Ticket QR of an older QR version:
	// the panel renewed the link since.
	ErrTicketQRSuperseded = errors.New("ticket: ticket qr superseded")
)

// TicketQRs mints and verifies Ticket QRs (*ticketqr.Signer).
type TicketQRs interface {
	Mint(c ticketqr.Claims) (string, error)
	Verify(token string) (ticketqr.Claims, error)
}

// QRExpiry is when t's Ticket QR stops opening the door: a day after ev ends
// (its end date, else a day after its start), or, for an Event with no date
// (which never ends, ADR 0066), a year after the Ticket was written.
func QRExpiry(t Ticket, ev event.Event) time.Time {
	return ticketqr.ExpiresAt(ev.EndsAt(), t.CreatedAt)
}

// held reports whether t still has a holder: an erased member's Ticket is
// detached (no owner) and an erased guest's has no address.
func held(t Ticket) bool {
	if t.TicketType == Guest {
		return strings.TrimSpace(t.GuestEmail) != ""
	}
	return t.OwnerID != nil
}

// CheckInTicketQR takes a check-in at the Session's door from a Ticket QR
// the scanner read. The door's own rule comes first, as for SkyPass
// (AuthorizeSessionDoor, before the code is read). Then: the signature (any
// failure ErrTicketQRInvalid, 401), the Event (another Event's code or no
// such Ticket ErrNotFound, 404), the expiry by the Event's dates now
// (ErrTicketQRExpired, 401), the QR version (ErrTicketQRSuperseded, 410),
// and the check-in itself (ErrConflict, 409, when the Ticket is already
// in).
func (s *service) CheckInTicketQR(ctx context.Context, p authz.Principal, sessionID uuid.UUID, token string) (CheckIn, error) {
	ci, err := s.checkInTicketQR(ctx, p, sessionID, token)
	s.scans.record(err)
	return ci, err
}

func (s *service) checkInTicketQR(ctx context.Context, p authz.Principal, sessionID uuid.UUID, token string) (CheckIn, error) {
	ev, err := s.authorizedDoorEvent(ctx, p, sessionID, true)
	if err != nil {
		return CheckIn{}, err
	}
	t, err := s.ticketOfQR(ctx, ev, token)
	if err != nil {
		return CheckIn{}, err
	}
	return s.checkIn(ctx, p, t.ID, sessionID, true)
}

// ticketOfQR is the Ticket of ev a Ticket QR the door scanned names: the
// signature (ErrTicketQRInvalid), the Event (ErrNotFound for another
// Event's or no such Ticket), the expiry by the Event's dates now
// (ErrTicketQRExpired) and the QR version (ErrTicketQRSuperseded).
func (s *service) ticketOfQR(ctx context.Context, ev event.Event, token string) (Ticket, error) {
	if s.qr == nil {
		return Ticket{}, ErrTicketQRInvalid
	}
	claims, err := s.qr.Verify(token)
	if err != nil {
		return Ticket{}, ErrTicketQRInvalid
	}
	if claims.Event != ev.ID {
		return Ticket{}, ErrNotFound
	}
	t, err := s.tickets.Get(ctx, claims.Ticket)
	if err != nil {
		return Ticket{}, err
	}
	if t.EventID != ev.ID || !held(t) {
		return Ticket{}, ErrNotFound
	}
	// Online, the Event's dates now decide, not the expiry the code was
	// signed with: an Event moved later keeps the codes already sent.
	if !time.Now().Before(QRExpiry(t, ev)) {
		return Ticket{}, ErrTicketQRExpired
	}
	if claims.Version != t.QRVersion {
		return Ticket{}, ErrTicketQRSuperseded
	}
	return t, nil
}

// TicketQR is t's Ticket QR, as the holder's mail, PDF, page and app show
// it: the same string until the QR version or the Event's dates change.
func (s *service) TicketQR(t Ticket, ev event.Event) (string, error) {
	if s.qr == nil {
		return "", ErrTicketQRInvalid
	}
	return s.qr.Mint(ticketqr.Claims{Ticket: t.ID, Event: ev.ID, Version: t.QRVersion, ExpiresAt: QRExpiry(t, ev)})
}

// doorTicketByCode is the Ticket of ev with the code query reads as, when it
// reads as one and a Ticket with a holder has it (the desk, docs/ticket-qr.md).
func (s *service) doorTicketByCode(ctx context.Context, eventID uuid.UUID, query string) (DoorTicketIdentity, bool, error) {
	code, ok := NormalizeCode(query)
	if !ok {
		return DoorTicketIdentity{}, false, nil
	}
	if store, ok := s.tickets.(CodeStore); ok {
		row, err := store.DoorTicketByCode(ctx, eventID, code)
		if errors.Is(err, ErrNotFound) {
			return DoorTicketIdentity{}, false, nil
		}
		return row, err == nil, err
	}
	listed, err := s.everyTicket(ctx, eventID)
	if err != nil {
		return DoorTicketIdentity{}, false, err
	}
	for _, candidate := range listed {
		if storedCode(candidate.Code) != code || !held(candidate) {
			continue
		}
		name, values, found := s.localDoorTicketIdentity(ctx, candidate)
		email := ""
		if len(values) > 0 {
			email = values[0]
		}
		return DoorTicketIdentity{Ticket: candidate, Name: name, Email: email, Found: found}, true, nil
	}
	return DoorTicketIdentity{}, false, nil
}

// CodeStore finds a Ticket of an Event by its code (stored, without the
// hyphen), for the desk: ErrNotFound when none, or when its holder was
// erased.
type CodeStore interface {
	DoorTicketByCode(ctx context.Context, eventID uuid.UUID, code string) (DoorTicketIdentity, error)
}

// scanOutcomes are the outcomes of a Ticket QR scan the metric counts.
var scanOutcomes = []string{"checked_in", "already", "not_found", "invalid", "expired", "superseded", "session_closed", "forbidden", "failed"}

// DoorScanMetrics counts Ticket QR scans by outcome:
// skylab_door_scans_total{credential="ticket_qr",outcome=…}; and the Venue
// entries written by source: skylab_venue_entries_total{source=…}
// (docs/venue-entry.md). Neither names anybody.
type DoorScanMetrics struct {
	counts  [9]atomic.Uint64
	entries [3]atomic.Uint64
}

// recordEntry counts a Venue entry written.
func (m *DoorScanMetrics) recordEntry(source EntrySource) {
	if m == nil {
		return
	}
	if i := slices.Index(entrySources, source); i >= 0 {
		m.entries[i].Add(1)
	}
}

func (m *DoorScanMetrics) record(err error) {
	if m == nil {
		return
	}
	i := 8
	switch {
	case err == nil:
		i = 0
	case errors.Is(err, ErrConflict):
		i = 1
	case errors.Is(err, ErrNotFound):
		i = 2
	case errors.Is(err, ErrTicketQRInvalid):
		i = 3
	case errors.Is(err, ErrTicketQRExpired):
		i = 4
	case errors.Is(err, ErrTicketQRSuperseded):
		i = 5
	case errors.Is(err, ErrSessionClosed), errors.Is(err, ErrSessionCancelled), errors.Is(err, ErrSessionNotPublished):
		i = 6
	case errors.Is(err, ErrForbidden):
		i = 7
	}
	m.counts[i].Add(1)
}

// Prometheus is the counters in the text exposition format.
func (m *DoorScanMetrics) Prometheus() string {
	if m == nil {
		return ""
	}
	var out strings.Builder
	out.WriteString("# TYPE skylab_door_scans_total counter\n")
	for i, outcome := range scanOutcomes {
		out.WriteString(`skylab_door_scans_total{credential="ticket_qr",outcome="` + outcome + `"} ` +
			strconv.FormatUint(m.counts[i].Load(), 10) + "\n")
	}
	out.WriteString("# TYPE skylab_venue_entries_total counter\n")
	for i, source := range entrySources {
		out.WriteString(`skylab_venue_entries_total{source="` + string(source) + `"} ` +
			strconv.FormatUint(m.entries[i].Load(), 10) + "\n")
	}
	return out.String()
}
