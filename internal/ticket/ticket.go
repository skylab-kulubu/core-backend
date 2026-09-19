package ticket

import (
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/event"
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
