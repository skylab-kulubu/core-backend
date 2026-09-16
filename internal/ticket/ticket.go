package ticket

import (
	"time"

	"github.com/google/uuid"
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
	ID               uuid.UUID  `json:"id"`
	EventID          uuid.UUID  `json:"eventId"`
	TicketType       string     `json:"ticketType"`
	OwnerID          *uuid.UUID `json:"ownerId,omitempty"`
	GuestFirstName   string     `json:"guestFirstName,omitempty"`
	GuestLastName    string     `json:"guestLastName,omitempty"`
	GuestEmail       string     `json:"guestEmail,omitempty"`
	GuestPhoneNumber string     `json:"guestPhoneNumber,omitempty"`
	CheckIns         []CheckIn  `json:"checkIns"`
	CreatedAt        time.Time  `json:"createdAt"`
	UpdatedAt        time.Time  `json:"updatedAt"`
}

type GuestInfo struct {
	FirstName   string
	LastName    string
	Email       string
	PhoneNumber string
}
