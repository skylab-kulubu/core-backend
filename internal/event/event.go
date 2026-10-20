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
	// Active is derived (ADR 0066): published and not ended (ActiveAt).
	// The store answers it; a write never sets it.
	Active bool `json:"active"`
	// Status is the stored status; only the status endpoints change it.
	Status      Status     `json:"status"`
	PublishedAt *time.Time `json:"publishedAt,omitempty"`
	// PublishedBy and CancelledBy name who did it, for the record; they
	// are not answered.
	PublishedBy  *uuid.UUID `json:"-"`
	CancelledAt  *time.Time `json:"cancelledAt,omitempty"`
	CancelledBy  *uuid.UUID `json:"-"`
	CancelReason string     `json:"cancelReason,omitempty"`
	// CancelNotifiedAt is when the current cancellation's mail to the
	// Ticket holders was queued (ticket 14): undoing it offers to tell them
	// again.
	CancelNotifiedAt *time.Time `json:"cancelNotifiedAt,omitempty"`
	// CopiedFromEventID is the Event this one was copied from.
	CopiedFromEventID *uuid.UUID `json:"copiedFromEventId,omitempty"`
	Ranked            bool       `json:"ranked"`
	PrizeInfo         string     `json:"prizeInfo,omitempty"`
	// FeedbackMode is where the Event asks for feedback; FeedbackFormURL
	// and FeedbackFormAlias are its feedback form and the short link alias
	// it gives it, a form whose answers write no Ticket; FeedbackAutoSend
	// mails the form when the Event ends (etkinlik geri bildirim formu,
	// ticket 02; docs/event-feedback.md).
	FeedbackMode      FeedbackMode `json:"feedbackMode"`
	FeedbackFormURL   string       `json:"feedbackFormUrl,omitempty"`
	FeedbackFormAlias string       `json:"feedbackFormAlias,omitempty"`
	FeedbackAutoSend  bool         `json:"feedbackAutoSend"`
	// RegistrationClosed is applying closed by hand, RegistrationClosesAt
	// the deadline: Registration closure, apart from the status (ADR 0066
	// addendum, ticket 12).
	RegistrationClosed   bool       `json:"registrationClosed"`
	RegistrationClosesAt *time.Time `json:"registrationClosesAt,omitempty"`
	// RegistrationClosedAt is when applying was closed by hand: set when
	// RegistrationClosed turns on, cleared when it turns off (the store
	// writes it). A forms answer sent after it earns no Ticket.
	RegistrationClosedAt *time.Time `json:"registrationClosedAt,omitempty"`
	// RegistrationOpen is derived like Active (RegistrationOpenAt): the
	// store answers it; a write never sets it.
	RegistrationOpen bool       `json:"registrationOpen"`
	SeasonID         *uuid.UUID `json:"seasonId,omitempty"`
	CoverImageID     *uuid.UUID `json:"coverImageId,omitempty"`
	CoverImageURL    string     `json:"coverImageUrl,omitempty"`
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

	// Files and Videos are the Event's files and videos, in their
	// organizers' order (media redesign ticket 22). Only an Event's detail
	// carries them: a list leaves them out (nil, omitzero) and answers
	// FileCount and VideoCount alone, which count the items anyone can see
	// (those that can be served, media.ServableSQL).
	Files      []MediaItem `json:"files,omitzero"`
	Videos     []MediaItem `json:"videos,omitzero"`
	FileCount  int         `json:"fileCount"`
	VideoCount int         `json:"videoCount"`

	// coverImage is the cover Media as the store read it with the Event,
	// what CoverImageSizes is built from.
	coverImage *media.LinkedImage
	// formGuard makes the store's Create and Update apply the one-place
	// rule of feedback forms again inside their write, under a lock per
	// form (feedback.go).
	formGuard *formGuard
	// cancelReasonSent makes the store's Update write CancelReason, which
	// it does only while the Event is cancelled: an update body that sends
	// cancelReason (Patch) sets it.
	cancelReasonSent bool
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
	Status          Status                        `json:"status"`
	PublishedAt     *time.Time                    `json:"publishedAt,omitempty"`
	CancelledAt     *time.Time                    `json:"cancelledAt,omitempty"`
	CancelReason    string                        `json:"cancelReason,omitempty"`
	// CopiedFromEventID is the Event this one was copied from.
	CopiedFromEventID *uuid.UUID `json:"copiedFromEventId,omitempty"`
	Ranked            bool       `json:"ranked"`

	// FileCount and VideoCount count the Event's files and videos anyone
	// can see; the Event's detail lists them.
	FileCount  int `json:"fileCount"`
	VideoCount int `json:"videoCount"`
}

// Resource is the Event's summary. Its callers (tickets, competitors, the
// door) have no media dependency to carry a base and address mode: the
// cover is answered under the ones core is configured with.
func (e Event) Resource() Resource {
	addresses := media.ConfiguredAddresses()
	return Resource{
		ID:                e.ID,
		Name:              e.Name,
		StartDate:         e.StartDate,
		EndDate:           e.EndDate,
		Location:          e.Location,
		OwnerTeam:         e.OwnerTeam,
		CoverImageURL:     addresses.Object(e.CoverImageURL),
		CoverImageSizes:   addresses.LinkedSizes(e.coverImage),
		CoverColors:       append([]string{}, e.CoverColors...),
		Active:            e.Active,
		Status:            e.Status,
		PublishedAt:       e.PublishedAt,
		CancelledAt:       e.CancelledAt,
		CancelReason:      e.CancelReason,
		CopiedFromEventID: e.CopiedFromEventID,
		Ranked:            e.Ranked,
		FileCount:         e.FileCount,
		VideoCount:        e.VideoCount,
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
