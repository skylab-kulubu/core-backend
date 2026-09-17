package event

import (
	"time"

	"github.com/google/uuid"
)

type Event struct {
	ID              uuid.UUID      `json:"id"`
	Name            string         `json:"name"`
	Description     string         `json:"description"`
	Location        string         `json:"location"`
	OwnerTeam       string         `json:"ownerTeam"`
	FormURL         string         `json:"formUrl,omitempty"`
	Capacity        int            `json:"capacity"`
	StartDate       *time.Time     `json:"startDate,omitempty"`
	EndDate         *time.Time     `json:"endDate,omitempty"`
	Linkedin        string         `json:"linkedin,omitempty"`
	Active          bool           `json:"active"`
	Ranked          bool           `json:"ranked"`
	PrizeInfo       string         `json:"prizeInfo,omitempty"`
	SeasonID        *uuid.UUID     `json:"seasonId,omitempty"`
	CoverImageID    *uuid.UUID     `json:"coverImageId,omitempty"`
	CoverImageURL   string         `json:"coverImageUrl,omitempty"`
	AttendanceRule  string         `json:"attendanceRule"`
	AttendanceRatio *float64       `json:"attendanceRatio,omitempty"`
	Images          []GalleryImage `json:"images"`
	ImageURLs       []string       `json:"imageUrls"`
	CreatedAt       time.Time      `json:"createdAt"`
	UpdatedAt       time.Time      `json:"updatedAt"`
}

type GalleryImage struct {
	ID  uuid.UUID `json:"id"`
	URL string    `json:"url,omitempty"`
}

type Resource struct {
	ID            uuid.UUID  `json:"id"`
	Name          string     `json:"name"`
	StartDate     *time.Time `json:"startDate,omitempty"`
	EndDate       *time.Time `json:"endDate,omitempty"`
	Location      string     `json:"location"`
	OwnerTeam     string     `json:"ownerTeam"`
	CoverImageURL string     `json:"coverImageUrl,omitempty"`
	Active        bool       `json:"active"`
	Ranked        bool       `json:"ranked"`
}

func (e Event) Resource() Resource {
	return Resource{
		ID:            e.ID,
		Name:          e.Name,
		StartDate:     e.StartDate,
		EndDate:       e.EndDate,
		Location:      e.Location,
		OwnerTeam:     e.OwnerTeam,
		CoverImageURL: e.CoverImageURL,
		Active:        e.Active,
		Ranked:        e.Ranked,
	}
}

func emptyGallery(e Event) Event {
	if e.Images == nil {
		e.Images = []GalleryImage{}
	}
	if e.ImageURLs == nil {
		e.ImageURLs = []string{}
	}
	return e
}

type Session struct {
	ID              uuid.UUID  `json:"id"`
	EventDayID      uuid.UUID  `json:"eventDayId"`
	Title           string     `json:"title"`
	SpeakerName     string     `json:"speakerName"`
	SpeakerLinkedin string     `json:"speakerLinkedin,omitempty"`
	Description     string     `json:"description,omitempty"`
	StartTime       *time.Time `json:"startTime,omitempty"`
	EndTime         *time.Time `json:"endTime,omitempty"`
	OrderIndex      int        `json:"orderIndex"`
	SessionType     string     `json:"sessionType"`
	Cancelled       bool       `json:"cancelled"`
}

type Day struct {
	ID        uuid.UUID  `json:"id"`
	EventID   uuid.UUID  `json:"eventId"`
	Name      string     `json:"name"`
	StartDate *time.Time `json:"startDate,omitempty"`
	EndDate   *time.Time `json:"endDate,omitempty"`
}
