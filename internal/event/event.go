package event

import (
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/media"
)

type Event struct {
	ID            uuid.UUID       `json:"id"`
	Name          string          `json:"name"`
	Description   string          `json:"description"`
	Location      string          `json:"location"`
	OwnerTeam     string          `json:"ownerTeam"`
	FormURL       string          `json:"formUrl,omitempty"`
	FormAlias     string          `json:"formAlias,omitempty"`
	ExtraFormURLs []EventFormLink `json:"extraFormUrls"`
	Capacity      int             `json:"capacity"`
	StartDate     *time.Time      `json:"startDate,omitempty"`
	EndDate       *time.Time      `json:"endDate,omitempty"`
	Linkedin      string          `json:"linkedin,omitempty"`
	Active        bool            `json:"active"`
	Ranked        bool            `json:"ranked"`
	PrizeInfo     string          `json:"prizeInfo,omitempty"`
	SeasonID      *uuid.UUID      `json:"seasonId,omitempty"`
	CoverImageID  *uuid.UUID      `json:"coverImageId,omitempty"`
	CoverImageURL string          `json:"coverImageUrl,omitempty"`
	// CoverImageSizes are the cover's card and page addresses, built like
	// the Media JSON's sizes.
	CoverImageSizes map[string]media.ImageAddress `json:"coverImageSizes,omitempty"`
	CoverColors     []string                      `json:"coverColors"`
	AttendanceRule  string                        `json:"attendanceRule"`
	AttendanceRatio *float64                      `json:"attendanceRatio,omitempty"`
	Images          []GalleryImage                `json:"images"`
	ImageURLs       []string                      `json:"imageUrls"`
	DoorStaffIDs    []uuid.UUID                   `json:"doorStaffIds,omitempty"`
	MailListID      *uuid.UUID                    `json:"mailListId,omitempty"`
	ArchivedAt      *time.Time                    `json:"archivedAt,omitempty"`
	ArchivedBy      *uuid.UUID                    `json:"archivedBy,omitempty"`
	CreatedAt       time.Time                     `json:"createdAt"`
	UpdatedAt       time.Time                     `json:"updatedAt"`

	// Files and Videos are the Event's downloads, in their organizers'
	// order (media redesign ticket 22). Only an Event's detail carries them:
	// a list leaves them out (nil, omitzero) and answers FileCount and
	// VideoCount alone, which count the items anyone can download.
	Files      []MediaItem `json:"files,omitzero"`
	Videos     []MediaItem `json:"videos,omitzero"`
	FileCount  int         `json:"fileCount"`
	VideoCount int         `json:"videoCount"`

	// coverImage is the cover Media as the store read it with the Event,
	// what CoverImageSizes is built from.
	coverImage *media.LinkedImage
}

type GalleryImage struct {
	ID  uuid.UUID `json:"id"`
	URL string    `json:"url,omitempty"`
	// Sizes are the image's card and page addresses, built like the Media
	// JSON's sizes.
	Sizes map[string]media.ImageAddress `json:"sizes,omitempty"`

	// image is the Media as the store read it with the gallery, what Sizes
	// is built from.
	image *media.LinkedImage
}

// Resource is the Event summary tickets, competitors and the door answer.
type Resource struct {
	ID            uuid.UUID  `json:"id"`
	Name          string     `json:"name"`
	StartDate     *time.Time `json:"startDate,omitempty"`
	EndDate       *time.Time `json:"endDate,omitempty"`
	Location      string     `json:"location"`
	OwnerTeam     string     `json:"ownerTeam"`
	CoverImageURL string     `json:"coverImageUrl,omitempty"`
	// CoverImageSizes are the cover's card and page addresses, built like
	// the Media JSON's sizes.
	CoverImageSizes map[string]media.ImageAddress `json:"coverImageSizes,omitempty"`
	CoverColors     []string                      `json:"coverColors"`
	Active          bool                          `json:"active"`
	Ranked          bool                          `json:"ranked"`

	// FileCount and VideoCount count the Event's files and videos anyone
	// can download; the Event's detail lists them.
	FileCount  int `json:"fileCount"`
	VideoCount int `json:"videoCount"`
}

// Resource is the Event's summary. Its callers (tickets, competitors, the
// door) have no media dependency to carry a base and address mode: the
// cover is answered under the ones core is configured with.
func (e Event) Resource() Resource {
	addresses := media.ConfiguredAddresses()
	return Resource{
		ID:              e.ID,
		Name:            e.Name,
		StartDate:       e.StartDate,
		EndDate:         e.EndDate,
		Location:        e.Location,
		OwnerTeam:       e.OwnerTeam,
		CoverImageURL:   addresses.Object(e.CoverImageURL),
		CoverImageSizes: addresses.LinkedSizes(e.coverImage),
		CoverColors:     append([]string{}, e.CoverColors...),
		Active:          e.Active,
		Ranked:          e.Ranked,
		FileCount:       e.FileCount,
		VideoCount:      e.VideoCount,
	}
}

func emptyGallery(e Event) Event {
	if e.Images == nil {
		e.Images = []GalleryImage{}
	}
	if e.ImageURLs == nil {
		e.ImageURLs = []string{}
	}
	if e.DoorStaffIDs == nil {
		e.DoorStaffIDs = []uuid.UUID{}
	}
	if e.ExtraFormURLs == nil {
		e.ExtraFormURLs = []EventFormLink{}
	}
	if e.CoverColors == nil {
		e.CoverColors = []string{}
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
	ArchivedAt      *time.Time `json:"archivedAt,omitempty"`
	ArchivedBy      *uuid.UUID `json:"archivedBy,omitempty"`
}

type Day struct {
	ID         uuid.UUID  `json:"id"`
	EventID    uuid.UUID  `json:"eventId"`
	Name       string     `json:"name"`
	StartDate  *time.Time `json:"startDate,omitempty"`
	EndDate    *time.Time `json:"endDate,omitempty"`
	ArchivedAt *time.Time `json:"archivedAt,omitempty"`
	ArchivedBy *uuid.UUID `json:"archivedBy,omitempty"`
}
