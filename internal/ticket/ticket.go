package ticket

import (
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

const (
	Registered = "REGISTERED"
	Guest      = "GUEST"
)

type CheckIn struct {
	ID         uuid.UUID `json:"id"`
	TicketID   uuid.UUID `json:"ticketId"`
	SessionID  uuid.UUID `json:"sessionId"`
	EventDayID uuid.UUID `json:"eventDayId"`
	CreatedAt  time.Time `json:"createdAt"`
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
}

// GuestCheckIn is a guest checking themselves in to a Session: the e-mail
// their guest Ticket was written for, and the door QR token they scanned
// (docs/guest-self-check-in.md).
type GuestCheckIn struct {
	Email     string
	DoorToken string
}

// DoorQR is a minted door QR for the screen at a Session's door. URL is what
// the QR encodes; the screen asks for a new one after RefreshAfterSeconds.
type DoorQR struct {
	Token               string    `json:"token"`
	URL                 string    `json:"url"`
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
	CheckIns         []CheckIn       `json:"checkIns"`
	CreatedAt        time.Time       `json:"createdAt"`
	UpdatedAt        time.Time       `json:"updatedAt"`
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
}
