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
	EventDayID uuid.UUID `json:"eventDayId"`
	CreatedAt  time.Time `json:"createdAt"`
}

type Ticket struct {
	ID               uuid.UUID       `json:"id"`
	EventID          uuid.UUID       `json:"eventId"`
	Event            *event.Resource `json:"event,omitempty"`
	TicketType       string          `json:"ticketType"`
	OwnerID          *uuid.UUID      `json:"ownerId,omitempty"`
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
