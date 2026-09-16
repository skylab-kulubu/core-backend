package event

import (
	"time"

	"github.com/google/uuid"
)

type Event struct {
	ID          uuid.UUID  `json:"id"`
	Name        string     `json:"name"`
	Description string     `json:"description"`
	Location    string     `json:"location"`
	OwnerTeam   string     `json:"ownerTeam"`
	FormURL     string     `json:"formUrl,omitempty"`
	Capacity    int        `json:"capacity"`
	StartDate   *time.Time `json:"startDate,omitempty"`
	EndDate     *time.Time `json:"endDate,omitempty"`
	Linkedin    string     `json:"linkedin,omitempty"`
	Active      bool       `json:"active"`
	Ranked      bool       `json:"ranked"`
	PrizeInfo   string     `json:"prizeInfo,omitempty"`
	CreatedAt   time.Time  `json:"createdAt"`
	UpdatedAt   time.Time  `json:"updatedAt"`
}

type Day struct {
	ID        uuid.UUID  `json:"id"`
	EventID   uuid.UUID  `json:"eventId"`
	Name      string     `json:"name"`
	StartDate *time.Time `json:"startDate,omitempty"`
	EndDate   *time.Time `json:"endDate,omitempty"`
}
