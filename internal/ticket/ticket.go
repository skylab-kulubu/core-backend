package ticket

import (
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/feedback"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

const (
	Registered = "REGISTERED"
	Guest      = "GUEST"
)

// Status is a Ticket's status (docs/ticket-cancellation.md). Only an active
// Ticket opens the door, is counted and is told of the Event; the others are
// kept, never deleted.
type Status string

const (
	StatusActive     Status = "active"
	StatusWaitlisted Status = "waitlisted"
	StatusCancelled  Status = "cancelled"
)

// Statuses are every status, in the order the panel lists them.
var Statuses = []Status{StatusActive, StatusWaitlisted, StatusCancelled}

// ValidStatus reports whether s is one of Statuses.
func ValidStatus(s Status) bool {
	return s == StatusActive || s == StatusWaitlisted || s == StatusCancelled
}

// CancelledBy is who cancelled a Ticket: its holder, an operator from the
// panel, or the form whose answer wrote it.
type CancelledBy string

const (
	CancelledByHolder   CancelledBy = "holder"
	CancelledByOperator CancelledBy = "operator"
	CancelledByForm     CancelledBy = "form"
)

// MaxCancelReason is the longest reason an operator may give, in characters.
const MaxCancelReason = 500

type CheckIn struct {
	ID         uuid.UUID `json:"id"`
	TicketID   uuid.UUID `json:"ticketId"`
	SessionID  uuid.UUID `json:"sessionId"`
	EventDayID uuid.UUID `json:"eventDayId"`
	CreatedAt  time.Time `json:"createdAt"`
	// ScannedAt is when the door took the scan, when it sent it later
	// (docs/offline-door.md); nil for a scan core took as it came.
	ScannedAt *time.Time `json:"scannedAt,omitempty"`
	// NoMail queues no door mail with it: a scan sent too late for one.
	NoMail bool `json:"-"`
	// Kind is what a door write answers: check_in, or entry when a scan in
	// the venue mode wrote the day's Venue entry alone (ID is then the
	// entry's; docs/venue-entry.md). A stored check-in leaves it out.
	Kind string `json:"kind,omitempty"`
	// Entry is the day's Venue entry AddCheckIn writes with the check-in
	// when the Ticket has none that day; nil writes none. EntryWritten
	// says whether it did.
	Entry        *VenueEntry `json:"-"`
	EntryWritten bool        `json:"-"`
}

type DoorCheckInTarget struct {
	PersonID *uuid.UUID
	Query    string
}

type DoorCheckIn struct {
	ID         uuid.UUID `json:"id"`
	TicketID   uuid.UUID `json:"-"`
	SessionID  uuid.UUID `json:"sessionId"`
	EventDayID uuid.UUID `json:"eventDayId"`
	PersonName string    `json:"personName"`
	CreatedAt  time.Time `json:"createdAt"`
	// Kind is check_in, or entry for the day's Venue entry the Session's
	// door activity lists in the venue mode (docs/venue-entry.md).
	Kind string `json:"kind"`
}

// GuestCheckIn is someone checking themselves in to a Session on the page
// the door or attendance QR opens (docs/guest-self-check-in.md): by the
// e-mail their Ticket was written for (a guest's, or a member's account
// e-mail: primary or school), or by their Ticket's access key (any Ticket),
// with the token of the QR they scanned.
type GuestCheckIn struct {
	Email     string
	TicketKey string
	DoorToken string
}

// GuestNotFoundError is a self check-in that found no Ticket: ErrNotFound,
// with where to register while the Event takes registrations (its form,
// or its external registration link). The page shows the link.
type GuestNotFoundError struct {
	RegistrationURL string
}

func (e *GuestNotFoundError) Error() string { return ErrNotFound.Error() }
func (e *GuestNotFoundError) Unwrap() error { return ErrNotFound }

// DoorQR is a minted door QR for the screen at a Session's door. URL is what
// the QR encodes; the screen asks for a new one after RefreshAfterSeconds.
//
// Use is the token's class ("door" or "display") and MaxUses how many
// check-ins one token admits. SessionQRMode is SESSION_QR_MODE: in "qr" the
// static Session QR no longer checks anyone in, so the panel stops offering
// it.
type DoorQR struct {
	Token               string    `json:"token"`
	URL                 string    `json:"url"`
	Use                 string    `json:"use"`
	MaxUses             int       `json:"maxUses"`
	SessionQRMode       string    `json:"sessionQrMode"`
	SessionID           uuid.UUID `json:"sessionId"`
	EventID             uuid.UUID `json:"eventId"`
	IssuedAt            time.Time `json:"issuedAt"`
	ExpiresAt           time.Time `json:"expiresAt"`
	RefreshAfterSeconds int       `json:"refreshAfterSeconds"`
}

type DoorActivity struct {
	Total int           `json:"total"`
	Items []DoorCheckIn `json:"items"`
}

type DoorAttendee struct {
	PersonID *uuid.UUID `json:"personId,omitempty"`
	Name     string     `json:"name"`
	Email    string     `json:"email"`
}

type DoorTicketIdentity struct {
	Ticket Ticket
	Name   string
	Email  string
	Found  bool
}

type PersonSummary struct {
	ID        uuid.UUID `json:"id"`
	Email     string    `json:"email"`
	FirstName string    `json:"firstName"`
	LastName  string    `json:"lastName"`
	// Status and DisplayName are set on a ticket's owner, as the user reads
	// answer them (identity.Person): a deletion-pending owner is answered
	// with the id, the status and "Silinmiş kullanıcı" only.
	Status      user.ReadStatus `json:"status,omitempty"`
	DisplayName string          `json:"displayName,omitempty"`
}

// ownerSummary is a ticket owner as core's row has them.
func ownerSummary(u user.User) PersonSummary {
	if u.AccountState != user.AccountActive {
		return PersonSummary{
			ID: u.ID, FirstName: user.DeletedDisplayName,
			Status: u.AccountState.ReadStatus(), DisplayName: user.DeletedDisplayName,
		}
	}
	return activeSummary(PersonSummary{ID: u.ID, Email: u.Email, FirstName: u.FirstName, LastName: u.LastName})
}

func activeSummary(summary PersonSummary) PersonSummary {
	summary.Status = user.ReadStatusActive
	summary.DisplayName = strings.TrimSpace(summary.FirstName + " " + summary.LastName)
	return summary
}

type Ticket struct {
	ID               uuid.UUID       `json:"id"`
	EventID          uuid.UUID       `json:"eventId"`
	Event            *event.Resource `json:"event,omitempty"`
	TicketType       string          `json:"ticketType"`
	OwnerID          *uuid.UUID      `json:"ownerId,omitempty"`
	Owner            *PersonSummary  `json:"owner,omitempty"`
	GuestFirstName   string          `json:"guestFirstName,omitempty"`
	GuestLastName    string          `json:"guestLastName,omitempty"`
	GuestEmail       string          `json:"guestEmail,omitempty"`
	GuestPhoneNumber string          `json:"guestPhoneNumber,omitempty"`
	// Code is the Ticket's code as shown, XXXX-XXXX: the holder's and the
	// panel's (docs/ticket-access-key-and-code.md). Not a credential.
	Code string `json:"code,omitempty"`
	// AccessKey opens the ticket page without a sign-in. Only the holder
	// is given it (their mail, GET /v1/tickets/me as a link); no answer
	// carries the key itself.
	AccessKey string `json:"-"`
	// QRVersion is the Ticket QR's version: renewing the link raises it,
	// and a QR of an older one is refused online.
	QRVersion int `json:"-"`
	// TicketURL and QRToken are the holder's own, in their own list
	// (GET /v1/tickets/me) only: the ticket page and the Ticket QR
	// (docs/ticket-page.md, docs/ticket-qr.md).
	TicketURL string `json:"ticketUrl,omitempty"`
	QRToken   string `json:"qrToken,omitempty"`
	// Cancellable is whether the holder may cancel the Ticket now
	// (HolderMayCancel; kontenjan-iptal-hatirlatma 03), in the holder's own
	// list (GET /v1/tickets/me) only; every other answer leaves it out.
	Cancellable *bool `json:"cancellable,omitempty"`
	// Status is active, waitlisted or cancelled; the empty status of a
	// Ticket made in memory reads as active (Active).
	Status          Status      `json:"status"`
	StatusChangedAt *time.Time  `json:"statusChangedAt,omitempty"`
	WaitlistedAt    *time.Time  `json:"waitlistedAt,omitempty"`
	CancelledAt     *time.Time  `json:"cancelledAt,omitempty"`
	CancelledBy     CancelledBy `json:"cancelledBy,omitempty"`
	CancelReason    string      `json:"cancelReason,omitempty"`
	// FormsRejectedAt is when Forms declined the answer that wrote the
	// Ticket (Yusuf, K6 2026-11-25): core no longer cancels such a Ticket
	// by itself; the panel shows "Forms'ta reddedildi" and offers to cancel
	// it. A later acceptance of the answer, or of the holder's next answer,
	// clears it (docs/form-response-tickets.md).
	FormsRejectedAt *time.Time `json:"formsRejectedAt,omitempty"`
	// FormsStatus is where the Ticket's forms answer stands in Forms, for
	// the panel's badge (docs/form-respondents.md): accepted, pending
	// ("Forms'ta yeniden onay bekliyor"; the Ticket stays) or declined (K6,
	// FormsRejectedAt); left out for a Ticket no forms answer wrote (one an
	// operator added).
	FormsStatus string `json:"formsStatus,omitempty"`
	// OverCapacity is set on the answer of an operator's write that made
	// the Ticket active past its Event's capacity (docs/event-capacity.md),
	// for the panel's warning. Never stored.
	OverCapacity bool      `json:"overCapacity,omitempty"`
	CheckIns     []CheckIn `json:"checkIns"`
	// Feedback are the holder's own feedback links (ticket 12): only the
	// holder's own tickets answer (`GET /v1/tickets/me`) carries them, and
	// only when there is one (docs/event-feedback.md).
	Feedback  []feedback.TicketLink `json:"feedback,omitempty"`
	CreatedAt time.Time             `json:"createdAt"`
	UpdatedAt time.Time             `json:"updatedAt"`
}

type GuestInfo struct {
	FirstName   string
	LastName    string
	Email       string
	PhoneNumber string
}

// GuestApplyResult is what a Guest apply did to the guest Ticket.
type GuestApplyResult string

const (
	// GuestCreated means the Event had no guest Ticket for the e-mail; one
	// was written.
	GuestCreated GuestApplyResult = "created"
	// GuestExisting means the Ticket was already there and the caller asked
	// for no change it may not make. A trusted caller's details were
	// written; anybody else's only filled what the Ticket lacked.
	GuestExisting GuestApplyResult = "existing"
	// GuestKept means the Ticket was already there and the caller, who may
	// not change an existing guest's details, sent a name or phone number
	// different from the stored one. The stored details were kept.
	GuestKept GuestApplyResult = "kept"
)

// GuestApplication is the outcome of a Guest apply.
type GuestApplication struct {
	Result GuestApplyResult
	// Trusted reports whether the caller is a product's service identity or
	// an operator of the Event. Only such a caller sees the Ticket and may
	// change an existing guest's details.
	Trusted bool
	// Ticket is the guest Ticket as stored, for a Trusted caller. Anybody
	// else gets the zero Ticket: not even its id.
	Ticket Ticket
	// Waitlisted is the answer's "waitlisted" (docs/event-capacity.md): the
	// Ticket's status for a Trusted caller; for anybody else what a new
	// guest would get on the Event now, so the answer does not tell whether
	// the e-mail had applied.
	Waitlisted bool
}

// Active reports whether t is an active Ticket: the only one that opens the
// door, is counted and is told of its Event.
func (t Ticket) Active() bool {
	return t.Status == StatusActive || t.Status == ""
}

// StatusOf is t's status, active for the empty one.
func (t Ticket) StatusOf() Status {
	if t.Status == "" {
		return StatusActive
	}
	return t.Status
}
