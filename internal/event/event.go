package event

import (
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/media"
	"github.com/skylab-kulubu/core-backend/internal/organizer"
)

type Event struct {
	ID          uuid.UUID `json:"id"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	Location    string    `json:"location"`
	// LocationLat and LocationLng are the place's point on the map (WGS 84
	// degrees, six decimals), both or neither; LocationAddress is its
	// street address. Location stays the place's name
	// (docs/event-location.md).
	LocationLat     *float64 `json:"locationLat,omitempty"`
	LocationLng     *float64 `json:"locationLng,omitempty"`
	LocationAddress string   `json:"locationAddress,omitempty"`
	// MapImageURL is the map of the place's point that core drew
	// (docs/event-map.md), MapAttribution the credit its data asks to be
	// shown under it and MapAttributionURL the page the credit links to;
	// all are left out while there is no map (no point, one being drawn,
	// or none to draw). MapImageID is its Media.
	MapImageURL       string          `json:"mapImageUrl,omitempty"`
	MapAttribution    string          `json:"mapAttribution,omitempty"`
	MapAttributionURL string          `json:"mapAttributionUrl,omitempty"`
	MapImageID        *uuid.UUID      `json:"-"`
	OwnerTeam         string          `json:"ownerTeam"`
	OrganizerName     string          `json:"organizerName"` // organizer.Name(OwnerTeam), set by answers, never stored
	FormURL           string          `json:"formUrl,omitempty"`
	FormAlias         string          `json:"formAlias,omitempty"`
	ExtraFormURLs     []EventFormLink `json:"extraFormUrls"`
	// ExternalRegistrationURL is where people register when the Event's
	// sign-up runs on another product's form (Microsoft Forms and the
	// like): an https address, left out for none. Anyone who sees the Event
	// sees it, for a "Kayıt ol" that opens it (docs/attendee-import.md).
	ExternalRegistrationURL string `json:"externalRegistrationUrl,omitempty"`
	// Capacity bounds the Event's active Tickets, 0 for no bound
	// (docs/event-capacity.md).
	Capacity int `json:"capacity"`
	// WaitlistEnabled puts a registration on the Event's waiting list once
	// its active Tickets fill Capacity; off, a full Event takes nobody new
	// but who an operator adds. Nil on a write keeps the stored one (on for
	// a new Event); the store always answers it. Waitlist reads it.
	WaitlistEnabled *bool `json:"waitlistEnabled"`
	// AttendanceWelcomeMail and AttendanceRecordedMail are the organizer's
	// switches for the door's mails (docs/attendance-mail.md): "hoş geldin"
	// on the day's first entry, "katılımın alındı" on each Session
	// Check-in. Nil on a write keeps the stored one (on for a new Event);
	// the store always answers them.
	AttendanceWelcomeMail  *bool `json:"attendanceWelcomeMail"`
	AttendanceRecordedMail *bool `json:"attendanceRecordedMail"`
	// FormResponsesAutoTicket is the organizer's "Otomatik bilet gönder"
	// (docs/form-respondents.md): on, an accepted answer to the Event's form
	// writes its Ticket as before; off, it is kept as an Unticketed
	// respondent for the organizer to send. Nil on a write keeps the stored
	// one (on for a new Event); the store always answers it.
	FormResponsesAutoTicket *bool `json:"formResponsesAutoTicket"`
	// AttendeeNote is the organizer's note for whoever enters (Wi-Fi, the
	// way to the room; docs/attendance-mail.md): plain text, at most
	// AttendeeNoteMax characters, "" for none. Nil on a write keeps the
	// stored one; the store always answers it. It is never in a JSON answer
	// by itself: ProjectFor copies it to AttendeeNoteView for who may edit
	// the Event, so a public answer cannot carry it by mistake.
	AttendeeNote     *string `json:"-"`
	AttendeeNoteView *string `json:"attendeeNote,omitempty"`
	// TicketNote is the organizer's announcement for the Ticket holders
	// (the event's WhatsApp group, say; docs/ticket-note.md): plain text,
	// at most TicketNoteMax characters, "" for none, its https addresses
	// links in the mail. The ticket mails carry it (event.ticket-created,
	// event.waitlist-promoted, event.reminder). Nil on a write keeps the
	// stored one; the store always answers it. Like the attendee note it
	// is never in a JSON answer by itself: ProjectFor copies it to
	// TicketNoteView for who reads the attendee note.
	TicketNote     *string `json:"-"`
	TicketNoteView *string `json:"ticketNote,omitempty"`
	// Full is derived by the store: Capacity > 0 and the active Tickets
	// fill it. Anyone who sees the Event sees it.
	Full bool `json:"full"`
	// TicketCount and WaitlistCount are the Event's active and waitlisted
	// Tickets: only to who may edit it (ProjectFor), left out otherwise.
	TicketCount   *int `json:"ticketCount,omitempty"`
	WaitlistCount *int `json:"waitlistCount,omitempty"`
	// ticketCount and waitlistCount are the counts as the store read them,
	// which ProjectFor answers to an editor alone.
	ticketCount, waitlistCount int
	// EntryMode is what a door scan writes (docs/venue-entry.md):
	// per_session, venue, or nil for automatic. EffectiveEntryMode is the
	// mode the door works in now, which the store derives from it and from
	// whether the Event has a live Session; a write never sets it.
	EntryMode          *EntryMode `json:"entryMode"`
	EffectiveEntryMode EntryMode  `json:"effectiveEntryMode"`
	StartDate          *time.Time `json:"startDate,omitempty"`
	EndDate            *time.Time `json:"endDate,omitempty"`
	Linkedin           string     `json:"linkedin,omitempty"`
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
	// Theme is the key of the Event's theme (docs/event-theme.md): the look
	// of its PDF ticket, its participants' mails and its ticket page; nil
	// for Kurumsal, and for a key the catalogue no longer has. A write takes
	// a key of the catalogue (ErrThemeUnknown otherwise); nil, "" or
	// "kurumsal" is Kurumsal. TicketAccentColor is the ticket page's colour
	// it gives, #RRGGBB in capitals, nil for none. Anyone who sees the
	// Event sees both.
	Theme             *string `json:"theme"`
	TicketAccentColor *string `json:"ticketAccentColor"`
	// bannerImage is the Media of the Event's own ticket image (its
	// poster, docs/ticket-image.md source upload) as the store read it:
	// what BannerURL is built from.
	bannerImage     *media.LinkedImage
	AttendanceRule  string   `json:"attendanceRule"`
	AttendanceRatio *float64 `json:"attendanceRatio,omitempty"`
	// AttendanceMinCount is the count rule's threshold M ("at least M of
	// the N counted Sessions"); null is the default, ceil(3N/4). Any other
	// rule has none (docs/certificate-rule.md).
	AttendanceMinCount *int `json:"attendanceMinCount,omitempty"`
	// AttendanceAutoFinalize closes the attendance by itself an hour after
	// the last live Session ends (docs/certificate-rule.md); on for a new
	// Event and a copy, off for the Events that had ended when it came
	// (migration 20261124010000). The organizer turns it off per Event.
	AttendanceAutoFinalize bool           `json:"attendanceAutoFinalize"`
	Images                 []GalleryImage `json:"images"`
	ImageURLs              []string       `json:"imageUrls"`
	DoorStaffIDs           []uuid.UUID    `json:"doorStaffIds,omitempty"`
	// CoOrganizers are the Event's Co-organizer teams and CoOrganizerIDs
	// its Co-organizer people (docs/co-organizers.md), as the store read
	// them; nil for none. Only SetOrganizers writes them: a Create, an
	// Update or a Patch never does. Neither is in a JSON answer by itself:
	// ProjectFor copies them to CoOrganizersView and CoOrganizerIDsView for
	// who may edit the Event, so a public answer cannot carry them by
	// mistake. CoOrganizerTeams are the team codes anyone may see
	// (PublicCoOrganizerTeams): never a helper team, never a person.
	CoOrganizers       []CoOrganizerTeam `json:"-"`
	CoOrganizerIDs     []uuid.UUID       `json:"-"`
	CoOrganizersView   []CoOrganizerTeam `json:"coOrganizers,omitempty"`
	CoOrganizerIDsView []uuid.UUID       `json:"coOrganizerIds,omitempty"`
	CoOrganizerTeams   []string          `json:"coOrganizerTeams,omitempty"`
	// CoOrganizerNames names CoOrganizerTeams as the site shows them
	// (organizer.CoNames): "WEBLAB · Web Geliştirme, ARTLAB"; left out for
	// none.
	CoOrganizerNames string `json:"coOrganizerNames,omitempty"`
	// Viewer is what the signed-in caller may do on the Event (ProjectFor
	// answers it); left out for who is signed out.
	Viewer     *Viewer    `json:"viewer,omitempty"`
	MailListID *uuid.UUID `json:"mailListId,omitempty"`
	ArchivedAt *time.Time `json:"archivedAt,omitempty"`
	ArchivedBy *uuid.UUID `json:"archivedBy,omitempty"`
	CreatedAt  time.Time  `json:"createdAt"`
	UpdatedAt  time.Time  `json:"updatedAt"`

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
	// notifyHolders is the editor's answer to "tell the Ticket holders?"
	// for this update (Patch.NotifyTicketHolders); nil when the body did
	// not say, which tells them, as before the question
	// (docs/event-updated-notification.md#the-editors-choice).
	notifyHolders *bool
	// changedBy is who saves the update, for the record of that answer.
	changedBy *uuid.UUID
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
	OrganizerName string     `json:"organizerName"` // organizer.Name(OwnerTeam)
	// CoOrganizerTeams and CoOrganizerNames are the Event's Co-organizer
	// teams shown outside the panel and their names
	// (docs/co-organizers.md); left out for none, never a helper team or a
	// person.
	CoOrganizerTeams []string `json:"coOrganizerTeams,omitempty"`
	CoOrganizerNames string   `json:"coOrganizerNames,omitempty"`
	CoverImageURL    string   `json:"coverImageUrl,omitempty"`
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
	// EffectiveEntryMode is the mode the door works in (Event's): the door
	// apps open the day's "Alana giriş" screen on venue.
	EffectiveEntryMode EntryMode `json:"effectiveEntryMode"`

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
		ID:                 e.ID,
		Name:               e.Name,
		StartDate:          e.StartDate,
		EndDate:            e.EndDate,
		Location:           e.Location,
		OwnerTeam:          e.OwnerTeam,
		OrganizerName:      organizer.Name(e.OwnerTeam),
		CoOrganizerTeams:   PublicCoOrganizerTeams(e),
		CoOrganizerNames:   organizer.CoNames(PublicCoOrganizerTeams(e)),
		CoverImageURL:      addresses.Object(e.CoverImageURL),
		CoverImageSizes:    addresses.LinkedSizes(e.coverImage),
		CoverColors:        append([]string{}, e.CoverColors...),
		Active:             e.Active,
		Status:             e.Status,
		PublishedAt:        e.PublishedAt,
		CancelledAt:        e.CancelledAt,
		CancelReason:       e.CancelReason,
		CopiedFromEventID:  e.CopiedFromEventID,
		Ranked:             e.Ranked,
		EffectiveEntryMode: e.EffectiveEntryMode,
		FileCount:          e.FileCount,
		VideoCount:         e.VideoCount,
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
	// CertificateCounted makes the Session one of the N the certificate
	// rule counts; CertificateRequired makes missing it refuse the
	// certificate, and is only set on a counted Session
	// (docs/certificate-rule.md). The service reads what a body sends of
	// them from WithCertificate.
	CertificateCounted  bool `json:"certificateCounted"`
	CertificateRequired bool `json:"certificateRequired"`
	// FeedbackFormURL and FeedbackFormAlias are the Session's own feedback
	// form and its short link alias, used in the per_session mode;
	// FeedbackAutoSend mails it when the Session ends (etkinlik geri
	// bildirim formu, ticket 03; docs/event-feedback.md).
	FeedbackFormURL   string     `json:"feedbackFormUrl,omitempty"`
	FeedbackFormAlias string     `json:"feedbackFormAlias,omitempty"`
	FeedbackAutoSend  bool       `json:"feedbackAutoSend"`
	ArchivedAt        *time.Time `json:"archivedAt,omitempty"`
	ArchivedBy        *uuid.UUID `json:"archivedBy,omitempty"`

	// formGuard makes the store's CreateSession and UpdateSession apply the
	// one-place rule again inside their write (feedback.go).
	formGuard *formGuard
	// certificate is what the body sent of the certificate marks
	// (WithCertificate).
	certificate SessionCertificate
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
