package handlers

import (
	"bytes"
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/lifecycle"
	"github.com/skylab-kulubu/core-backend/internal/teamaudit"
)

type EventHandler struct {
	svc event.Service
}

func NewEventHandler(svc event.Service) *EventHandler {
	return &EventHandler{svc: svc}
}

type eventBody struct {
	ID          *uuid.UUID `json:"id"`
	Name        string     `json:"name"`
	Description string     `json:"description"`
	Location    string     `json:"location"`
	// The place's point and address (docs/event-location.md): the point
	// both or neither.
	LocationLat     *float64               `json:"locationLat"`
	LocationLng     *float64               `json:"locationLng"`
	LocationAddress string                 `json:"locationAddress"`
	OwnerTeam       string                 `json:"ownerTeam"`
	FormURL         string                 `json:"formUrl"`
	FormAlias       *string                `json:"formAlias"`
	ExtraFormURLs   *[]event.EventFormLink `json:"extraFormUrls"`
	// ExternalRegistrationURL on create: an https address, none for none
	// (docs/attendee-import.md).
	ExternalRegistrationURL string `json:"externalRegistrationUrl"`
	Capacity                int    `json:"capacity"`
	// WaitlistEnabled on create; none is on (docs/event-capacity.md).
	WaitlistEnabled *bool `json:"waitlistEnabled"`
	// AttendanceWelcomeMail and AttendanceRecordedMail on create; none is
	// on (docs/attendance-mail.md).
	AttendanceWelcomeMail  *bool `json:"attendanceWelcomeMail"`
	AttendanceRecordedMail *bool `json:"attendanceRecordedMail"`
	// FormResponsesAutoTicket on create; none is on
	// (docs/form-respondents.md).
	FormResponsesAutoTicket *bool `json:"formResponsesAutoTicket"`
	// AttendeeNote: the organizer's note for whoever enters; none keeps it
	// (docs/attendance-mail.md).
	AttendeeNote *string `json:"attendeeNote"`
	// EntryMode on create: per_session, venue, or none for automatic
	// (docs/venue-entry.md).
	EntryMode *event.EntryMode `json:"entryMode"`
	StartDate *time.Time       `json:"startDate"`
	EndDate   *time.Time       `json:"endDate"`
	Linkedin  string           `json:"linkedin"`
	// Status is read on create alone (draft or published; none is
	// published). An update (event.Patch) reads neither it nor active,
	// which is derived (ADR 0066): a client that sends the whole Event
	// back cannot change the status with it.
	Status          string     `json:"status"`
	Ranked          bool       `json:"ranked"`
	PrizeInfo       string     `json:"prizeInfo"`
	CoverImageID    *uuid.UUID `json:"coverImageId"`
	AttendanceRule  string     `json:"attendanceRule"`
	AttendanceRatio *float64   `json:"attendanceRatio"`
	// AttendanceMinCount is the count rule's threshold, none for the
	// default (docs/certificate-rule.md).
	AttendanceMinCount *int `json:"attendanceMinCount"`
	// AttendanceAutoFinalize on create; none is on
	// (docs/certificate-rule.md).
	AttendanceAutoFinalize *bool        `json:"attendanceAutoFinalize"`
	DoorStaffIDs           *[]uuid.UUID `json:"doorStaffIds"`
	// Registration closure on create (ticket 12); none is open.
	RegistrationClosed   bool       `json:"registrationClosed"`
	RegistrationClosesAt *time.Time `json:"registrationClosesAt"`
	// The Event's feedback on create (etkinlik geri bildirim formu, ticket
	// 02): no mode is event, no feedbackAutoSend is true.
	FeedbackMode      string `json:"feedbackMode"`
	FeedbackFormURL   string `json:"feedbackFormUrl"`
	FeedbackFormAlias string `json:"feedbackFormAlias"`
	FeedbackAutoSend  *bool  `json:"feedbackAutoSend"`
}

func eventError(c fiber.Ctx, err error) error {
	if handled, problemErr := linkProblem(c, err); handled {
		return problemErr
	}
	var incomplete *event.IncompleteError
	var inUse *event.FormInUseError
	switch {
	case errors.As(err, &incomplete):
		return problemWithFields(c, fiber.StatusBadRequest, "Bad Request", "Bad Request", "event_incomplete",
			fiber.Map{"missing": incomplete.Missing})
	case errors.As(err, &inUse):
		return problemWithFields(c, fiber.StatusConflict, "Conflict", "Conflict", "form_in_use", formInUseFields(inUse))
	case errors.Is(err, fiber.ErrUnauthorized):
		return problem(c, fiber.StatusUnauthorized, "Unauthorized")
	case errors.Is(err, teamaudit.ErrUnavailable):
		// A copy's Co-organizer rows could not be opened: no copy was made.
		return problemCode(c, fiber.StatusServiceUnavailable, "Service Unavailable", auditUnavailableCode)
	case errors.Is(err, event.ErrCoOrganizerPersonDates):
		return problemCode(c, fiber.StatusForbidden, "Forbidden", "co_organizer_person_dates")
	case errors.Is(err, event.ErrForbidden):
		return problem(c, fiber.StatusForbidden, "Forbidden")
	case errors.Is(err, event.ErrNotFound):
		return problem(c, fiber.StatusNotFound, "Not Found")
	case errors.Is(err, event.ErrConflict):
		return problemCode(c, fiber.StatusConflict, "Conflict", eventStatusCode(err))
	case errors.Is(err, event.ErrAttendeeNoteInvalid):
		return problemCode(c, fiber.StatusBadRequest, "Bad Request", "event_attendee_note_invalid")
	case errors.Is(err, event.ErrLocationInvalid):
		return problemCode(c, fiber.StatusBadRequest, "Bad Request", "event_location_invalid")
	case errors.Is(err, event.ErrExternalRegistrationURLInvalid):
		return problemCode(c, fiber.StatusBadRequest, "Bad Request", "external_registration_url_invalid")
	case errors.Is(err, event.ErrInvalid):
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	default:
		return err
	}
}

// formInUseFields says which form a form_in_use refusal is about and, when
// the caller may see it, the current Event that holds it (ADR 0069,
// addendum of 2026-11-22): the panel names it and offers a new form.
func formInUseFields(inUse *event.FormInUseError) fiber.Map {
	fields := fiber.Map{}
	if inUse.FormID != uuid.Nil {
		fields["formId"] = inUse.FormID
	}
	if h := inUse.Holder; h != nil {
		fields["event"] = fiber.Map{"id": h.ID, "name": h.Name, "status": h.Status}
	}
	return fields
}

func (b eventBody) asEvent() event.Event {
	e := event.Event{
		Name:            b.Name,
		Description:     b.Description,
		Location:        b.Location,
		LocationLat:     b.LocationLat,
		LocationLng:     b.LocationLng,
		LocationAddress: b.LocationAddress,
		OwnerTeam:       b.OwnerTeam,
		FormURL:         b.FormURL,
		Capacity:        b.Capacity,

		ExternalRegistrationURL: b.ExternalRegistrationURL,

		WaitlistEnabled: b.WaitlistEnabled,
		EntryMode:       b.EntryMode,

		AttendanceWelcomeMail:   b.AttendanceWelcomeMail,
		AttendanceRecordedMail:  b.AttendanceRecordedMail,
		FormResponsesAutoTicket: b.FormResponsesAutoTicket,
		AttendeeNote:            b.AttendeeNote,
		StartDate:               b.StartDate,
		EndDate:                 b.EndDate,
		Linkedin:                b.Linkedin,
		Status:                  event.Status(b.Status),
		Ranked:                  b.Ranked,
		PrizeInfo:               b.PrizeInfo,
		CoverImageID:            b.CoverImageID,
		AttendanceRule:          b.AttendanceRule,
		AttendanceRatio:         b.AttendanceRatio,

		AttendanceMinCount:     b.AttendanceMinCount,
		AttendanceAutoFinalize: b.AttendanceAutoFinalize == nil || *b.AttendanceAutoFinalize,

		RegistrationClosed:   b.RegistrationClosed,
		RegistrationClosesAt: b.RegistrationClosesAt,

		FeedbackMode:      event.FeedbackMode(b.FeedbackMode),
		FeedbackFormURL:   strings.TrimSpace(b.FeedbackFormURL),
		FeedbackFormAlias: strings.TrimSpace(b.FeedbackFormAlias),
		FeedbackAutoSend:  b.FeedbackAutoSend == nil || *b.FeedbackAutoSend,
	}
	if b.FormAlias != nil {
		e.FormAlias = *b.FormAlias
	}
	if b.ExtraFormURLs != nil {
		e.ExtraFormURLs = *b.ExtraFormURLs
	}
	if b.ID != nil {
		e.ID = *b.ID
	}
	if b.DoorStaffIDs != nil {
		e.DoorStaffIDs = *b.DoorStaffIDs
	}
	return e
}

// draftsQuery reads ?drafts=: include adds the drafts the caller may see
// (ADR 0066); none or exclude leaves them out.
func draftsQuery(c fiber.Ctx) (bool, error) {
	switch c.Query("drafts") {
	case "", "exclude":
		return false, nil
	case "include":
		return true, nil
	default:
		return false, event.ErrInvalid
	}
}

// eventListFilterInvalid refuses a list whose ?team= or ?managed= cannot
// be read or meets a filter it does not go with (docs/co-organizers.md).
const eventListFilterInvalid = "event_list_filter_invalid"

// teamFilters reads ?team= (an Event whose Owner team or shown
// Co-organizer team it is) and ?managed= (true: the Events the caller may
// edit). Neither goes with the archive (?lifecycle= other than current),
// and ?team= not with ?ownerTeam=.
func teamFilters(c fiber.Ctx, owner string, visibility lifecycle.Visibility) (team string, managed bool, err error) {
	team = strings.TrimSpace(c.Query("team"))
	switch c.Query("managed") {
	case "", "false":
	case "true":
		managed = true
	default:
		return "", false, event.ErrInvalid
	}
	if (team != "" || managed) && visibility != lifecycle.CurrentOnly {
		return "", false, event.ErrInvalid
	}
	if team != "" && strings.TrimSpace(owner) != "" {
		return "", false, event.ErrInvalid
	}
	return team, managed, nil
}

// withoutCurrentDrafts leaves out the drafts that are not archived:
// lifecycle=all is the current list and the archive together, and the
// current list carries a draft only with ?drafts=include (ADR 0066). An
// archived draft stays: the archive (lifecycle=inactive) is where a deleted
// draft is restored from.
func withoutCurrentDrafts(events []event.Event) []event.Event {
	out := make([]event.Event, 0, len(events))
	for _, e := range events {
		if e.Status == event.StatusDraft && e.ArchivedAt == nil {
			continue
		}
		out = append(out, e)
	}
	return out
}

func (h *EventHandler) List(c fiber.Ctx) error {
	visibility, err := lifecycleVisibility(c)
	if err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	drafts, err := draftsQuery(c)
	if err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	owner := c.Query("ownerTeam")
	activeOnly := c.Query("active") == "true"
	team, managed, err := teamFilters(c, owner, visibility)
	if err != nil {
		return problemCode(c, fiber.StatusBadRequest, "Bad Request", eventListFilterInvalid)
	}
	p, callerErr := caller(c)
	if managed {
		if callerErr != nil {
			return eventError(c, callerErr)
		}
		events, err := h.svc.ListManaged(c.Context(), p, owner, team, activeOnly)
		if err != nil {
			return eventError(c, err)
		}
		return c.JSON(h.svc.ProjectAllFor(&p, events))
	}
	if team != "" {
		if callerErr != nil {
			events, err := h.svc.ListTeam(c.Context(), nil, team, true, false)
			if err != nil {
				return eventError(c, err)
			}
			return c.JSON(h.svc.ProjectAllFor(nil, events))
		}
		events, err := h.svc.ListTeam(c.Context(), &p, team, activeOnly, drafts)
		if err != nil {
			return eventError(c, err)
		}
		return c.JSON(h.svc.ProjectAllFor(&p, events))
	}
	if visibility != lifecycle.CurrentOnly {
		if callerErr != nil {
			return eventError(c, callerErr)
		}
		events, err := h.svc.ListLifecycle(c.Context(), p, owner, visibility)
		if err != nil {
			return eventError(c, err)
		}
		if visibility == lifecycle.All && !drafts {
			events = withoutCurrentDrafts(events)
		}
		return c.JSON(h.svc.ProjectAllFor(&p, events))
	}
	if callerErr != nil {
		events, err := h.svc.List(c.Context(), nil, owner, true, false)
		if err != nil {
			return eventError(c, err)
		}
		return c.JSON(h.svc.ProjectAllFor(nil, events))
	}
	events, err := h.svc.List(c.Context(), &p, owner, activeOnly, drafts)
	if err != nil {
		return eventError(c, err)
	}
	return c.JSON(h.svc.ProjectAllFor(&p, events))
}

func (h *EventHandler) Restore(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return eventError(c, err)
	}
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	restored, err := h.svc.Restore(c.Context(), p, id)
	if err != nil {
		return eventError(c, err)
	}
	return c.JSON(h.svc.ProjectFor(&p, restored))
}

// Places is GET /v1/events/places: the places Events were held at, for the
// panel's place field (docs/event-location.md). ownerTeam picks a team's;
// none is the club's, for event:manage. limit is 1–50, 20 by default.
func (h *EventHandler) Places(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return eventError(c, err)
	}
	limit := 0
	if raw := c.Query("limit"); raw != "" {
		if limit, err = strconv.Atoi(raw); err != nil || limit < 1 {
			return problem(c, fiber.StatusBadRequest, "Bad Request")
		}
	}
	places, err := h.svc.Places(c.Context(), p, strings.TrimSpace(c.Query("ownerTeam")), limit)
	if err != nil {
		return eventError(c, err)
	}
	return c.JSON(places)
}

func (h *EventHandler) ListActive(c fiber.Ctx) error {
	events, err := h.svc.List(c.Context(), nil, "", true, false)
	if err != nil {
		return eventError(c, err)
	}
	return c.JSON(h.svc.ProjectAllFor(nil, events))
}

func (h *EventHandler) Get(c fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	p := callerOrNil(c)
	ev, err := h.svc.Get(c.Context(), p, id)
	if err != nil {
		return eventError(c, err)
	}
	return c.JSON(h.svc.ProjectFor(p, ev))
}

// publishBody is the body of POST /v1/events/{id}/publish, optional.
type publishBody struct {
	// NotifyTicketHolders queues the "your ticket is ready" Notification
	// (event.ticket-created) of the Tickets written while the Event was a
	// draft (Yusuf, 2026-11-22, K3). A body without it mails nobody: an
	// older client (sky-app) does not start a mail.
	NotifyTicketHolders bool `json:"notifyTicketHolders"`
}

// Publish publishes a draft: POST /v1/events/{id}/publish (ADR 0066).
func (h *EventHandler) Publish(c fiber.Ctx) error {
	var body publishBody
	if len(bytes.TrimSpace(c.Body())) > 0 {
		if err := c.Bind().Body(&body); err != nil {
			return problem(c, fiber.StatusBadRequest, "Bad Request")
		}
	}
	return h.statusChange(c, func(ctx context.Context, p authz.Principal, id uuid.UUID) (event.Event, error) {
		return h.svc.Publish(ctx, p, id, body.NotifyTicketHolders)
	})
}

// Unpublish makes a published Event without Tickets a draft again: POST
// /v1/events/{id}/unpublish.
func (h *EventHandler) Unpublish(c fiber.Ctx) error {
	return h.statusChange(c, h.svc.Unpublish)
}

// cancelBody is the body of POST /v1/events/{id}/cancel, both fields
// optional; no body is none of them.
type cancelBody struct {
	Reason string `json:"reason"`
	// NotifyTicketHolders queues the cancellation mail to the Ticket
	// holders (ADR 0066 addendum 2026-10-19; ticket 14). A body without it
	// mails nobody: an older client does not start a mail.
	NotifyTicketHolders bool `json:"notifyTicketHolders"`
}

// Cancel cancels a published Event that has not ended: POST
// /v1/events/{id}/cancel (ADR 0066, ticket 04).
func (h *EventHandler) Cancel(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return eventError(c, err)
	}
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	var body cancelBody
	if len(bytes.TrimSpace(c.Body())) > 0 {
		if err := c.Bind().Body(&body); err != nil {
			return problem(c, fiber.StatusBadRequest, "Bad Request")
		}
	}
	cancelled, err := h.svc.Cancel(c.Context(), p, id, body.Reason, body.NotifyTicketHolders)
	if err != nil {
		return eventError(c, err)
	}
	return c.JSON(h.svc.ProjectFor(&p, cancelled))
}

// uncancelBody is the body of POST /v1/events/{id}/uncancel, optional.
type uncancelBody struct {
	// NotifyTicketHolders queues the mail that the Event is back to whom
	// the cancellation mail reached (ticket 14); without a cancellation
	// mail it does nothing.
	NotifyTicketHolders bool `json:"notifyTicketHolders"`
}

// Uncancel undoes a cancellation: POST /v1/events/{id}/uncancel (ticket 15).
func (h *EventHandler) Uncancel(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return eventError(c, err)
	}
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	var body uncancelBody
	if len(bytes.TrimSpace(c.Body())) > 0 {
		if err := c.Bind().Body(&body); err != nil {
			return problem(c, fiber.StatusBadRequest, "Bad Request")
		}
	}
	uncancelled, err := h.svc.Uncancel(c.Context(), p, id, body.NotifyTicketHolders)
	if err != nil {
		return eventError(c, err)
	}
	return c.JSON(h.svc.ProjectFor(&p, uncancelled))
}

// copyBody is the body of POST /v1/events/{id}/copy (ticket 06).
type copyBody struct {
	// ID is the copy's id, which the client makes: a retried request
	// answers the copy it made.
	ID                  *uuid.UUID `json:"id"`
	Name                string     `json:"name"`
	StartDate           *time.Time `json:"startDate"`
	IncludeDoorStaff    bool       `json:"includeDoorStaff"`
	IncludeCompetitions bool       `json:"includeCompetitions"`
	// IncludeExternalRegistration and IncludeAttendeeNote copy the
	// source's external registration link and attendee note; none copies
	// neither.
	IncludeExternalRegistration bool `json:"includeExternalRegistration"`
	IncludeAttendeeNote         bool `json:"includeAttendeeNote"`
	// OwnerTeam is the copy's Owner team (ticket 16); none is the
	// source's.
	OwnerTeam string `json:"ownerTeam"`
	// IncludeCoOrganizerPeople copies the source's Co-organizer people
	// (docs/co-organizers.md); its Co-organizer teams always come along.
	IncludeCoOrganizerPeople bool `json:"includeCoOrganizerPeople"`
	// IncludeCertificateTemplate false leaves the certificate template
	// out (its Event template and binding); none copies it.
	IncludeCertificateTemplate *bool `json:"includeCertificateTemplate"`
}

// Copy copies an Event's structure into a new draft: POST
// /v1/events/{id}/copy (ADR 0066, ticket 06). 201 with the copy and the
// parts left out; 200 when the same id from the same source made it
// already.
func (h *EventHandler) Copy(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return eventError(c, err)
	}
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	var body copyBody
	if err := c.Bind().Body(&body); err != nil || body.ID == nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	// The Co-organizers a copy carries are recorded (docs/co-organizers.md).
	copied, fresh, err := h.svc.Copy(auditContext(c, p), p, id, event.CopyInput{
		ID:                          *body.ID,
		Name:                        body.Name,
		StartDate:                   body.StartDate,
		IncludeDoorStaff:            body.IncludeDoorStaff,
		IncludeCompetitions:         body.IncludeCompetitions,
		OwnerTeam:                   body.OwnerTeam,
		SkipCertificateTemplate:     body.IncludeCertificateTemplate != nil && !*body.IncludeCertificateTemplate,
		IncludeExternalRegistration: body.IncludeExternalRegistration,
		IncludeAttendeeNote:         body.IncludeAttendeeNote,
		IncludeCoOrganizerPeople:    body.IncludeCoOrganizerPeople,
	})
	if err != nil {
		return eventError(c, err)
	}
	copied.Event = h.svc.ProjectFor(&p, copied.Event)
	status := fiber.StatusOK
	if fresh {
		status = fiber.StatusCreated
	}
	return c.Status(status).JSON(copied)
}

func (h *EventHandler) statusChange(c fiber.Ctx, change func(ctx context.Context, p authz.Principal, id uuid.UUID) (event.Event, error)) error {
	p, err := caller(c)
	if err != nil {
		return eventError(c, err)
	}
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	changed, err := change(c.Context(), p, id)
	if err != nil {
		return eventError(c, err)
	}
	return c.JSON(h.svc.ProjectFor(&p, changed))
}

func (h *EventHandler) Create(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return eventError(c, err)
	}
	var body eventBody
	if err := c.Bind().Body(&body); err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	created, err := h.svc.Create(c.Context(), p, body.asEvent())
	if err != nil {
		return eventError(c, err)
	}
	return c.Status(fiber.StatusCreated).JSON(h.svc.ProjectFor(&p, created))
}

func (h *EventHandler) Update(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return eventError(c, err)
	}
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	// PUT and PATCH alike write only the fields the body sends (ticket 13).
	var body event.Patch
	if err := c.Bind().Body(&body); err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	updated, err := h.svc.Patch(c.Context(), p, id, body)
	if err != nil {
		return eventError(c, err)
	}
	return c.JSON(h.svc.ProjectFor(&p, updated))
}

func (h *EventHandler) Delete(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return eventError(c, err)
	}
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	if err := h.svc.Delete(c.Context(), p, id); err != nil {
		return eventError(c, err)
	}
	return c.SendStatus(fiber.StatusNoContent)
}

func (h *EventHandler) AddImages(c fiber.Ctx) error {
	return h.eventMediaChange(c, func(p authz.Principal, id uuid.UUID, ids []uuid.UUID) (event.Event, error) {
		return h.svc.AddImages(c.Context(), p, id, ids)
	})
}

func (h *EventHandler) RemoveImages(c fiber.Ctx) error {
	return h.eventMediaChange(c, func(p authz.Principal, id uuid.UUID, ids []uuid.UUID) (event.Event, error) {
		return h.svc.RemoveImages(c.Context(), p, id, ids)
	})
}

// eventMediaChange changes the Media an Event links (its gallery, files or
// videos) with the Media ids of the body, a JSON array, and answers the
// Event.
func (h *EventHandler) eventMediaChange(c fiber.Ctx, change func(p authz.Principal, id uuid.UUID, ids []uuid.UUID) (event.Event, error)) error {
	p, err := caller(c)
	if err != nil {
		return eventError(c, err)
	}
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	var ids []uuid.UUID
	if err := c.Bind().Body(&ids); err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	updated, err := change(p, id, ids)
	if err != nil {
		return eventError(c, err)
	}
	return c.JSON(h.svc.ProjectFor(&p, updated))
}

// AddFiles appends Media to the Event's list: POST /v1/events/{id}/files
// (club files) or /videos, with the Media ids in the order to add them.
func (h *EventHandler) AddFiles(list event.MediaList) fiber.Handler {
	return func(c fiber.Ctx) error {
		return h.eventMediaChange(c, func(p authz.Principal, id uuid.UUID, ids []uuid.UUID) (event.Event, error) {
			return h.svc.AddFiles(c.Context(), p, id, list, ids)
		})
	}
}

// RemoveFiles removes Media from the Event's list: DELETE
// /v1/events/{id}/files or /videos, with the Media ids.
func (h *EventHandler) RemoveFiles(list event.MediaList) fiber.Handler {
	return func(c fiber.Ctx) error {
		return h.eventMediaChange(c, func(p authz.Principal, id uuid.UUID, ids []uuid.UUID) (event.Event, error) {
			return h.svc.RemoveFiles(c.Context(), p, id, list, ids)
		})
	}
}

// OrderFiles orders the Event's list: PUT /v1/events/{id}/files/order or
// /videos/order, with every item's Media id in the new order.
func (h *EventHandler) OrderFiles(list event.MediaList) fiber.Handler {
	return func(c fiber.Ctx) error {
		return h.eventMediaChange(c, func(p authz.Principal, id uuid.UUID, ids []uuid.UUID) (event.Event, error) {
			return h.svc.OrderFiles(c.Context(), p, id, list, ids)
		})
	}
}

// posterBody names a video's new poster: an image Media uploaded for an
// Event (event_cover or event_gallery).
type posterBody struct {
	PosterID *uuid.UUID `json:"posterId"`
}

// SetVideoPoster sets or replaces a video's poster: PUT
// /v1/events/{id}/videos/{mediaId}/poster with {"posterId": "…"}.
func (h *EventHandler) SetVideoPoster(c fiber.Ctx) error {
	return h.videoPosterChange(c, func() (*uuid.UUID, bool) {
		var body posterBody
		if err := c.Bind().Body(&body); err != nil || body.PosterID == nil {
			return nil, false
		}
		return body.PosterID, true
	})
}

// ClearVideoPoster clears a video's poster: DELETE
// /v1/events/{id}/videos/{mediaId}/poster.
func (h *EventHandler) ClearVideoPoster(c fiber.Ctx) error {
	return h.videoPosterChange(c, func() (*uuid.UUID, bool) { return nil, true })
}

// videoPosterChange gives the video the path names the poster the request
// names (nil clears it), and answers the Event.
func (h *EventHandler) videoPosterChange(c fiber.Ctx, poster func() (*uuid.UUID, bool)) error {
	p, err := caller(c)
	if err != nil {
		return eventError(c, err)
	}
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	videoID, err := uuid.Parse(c.Params("mediaId"))
	if err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	posterID, ok := poster()
	if !ok {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	updated, err := h.svc.SetVideoPoster(c.Context(), p, id, videoID, posterID)
	if err != nil {
		return eventError(c, err)
	}
	return c.JSON(h.svc.ProjectFor(&p, updated))
}

// certificateRuleBody is the body of PUT /v1/events/{id}/certificate-rule
// (docs/certificate-rule.md): each listed Session says both of its marks.
type certificateRuleBody struct {
	Rule     string `json:"rule"`
	MinCount *int   `json:"minCount"`
	// AutoFinalize turns the automatic attendance finalization on or off;
	// left out (or null) keeps it.
	AutoFinalize *bool `json:"autoFinalize"`
	Sessions     []struct {
		ID       uuid.UUID `json:"id"`
		Counted  *bool     `json:"counted"`
		Required *bool     `json:"required"`
	} `json:"sessions"`
}

func (h *EventHandler) CertificateRule(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return eventError(c, err)
	}
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	rule, err := h.svc.CertificateRule(c.Context(), p, id)
	if err != nil {
		return eventError(c, err)
	}
	return c.JSON(rule)
}

func (h *EventHandler) SetCertificateRule(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return eventError(c, err)
	}
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	var body certificateRuleBody
	if err := c.Bind().Body(&body); err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	in := event.CertificateRuleInput{Rule: body.Rule, MinCount: body.MinCount, AutoFinalize: body.AutoFinalize, Sessions: make([]event.SessionCertificateFlags, 0, len(body.Sessions))}
	for _, sess := range body.Sessions {
		if sess.Counted == nil || sess.Required == nil {
			return problem(c, fiber.StatusBadRequest, "Bad Request")
		}
		in.Sessions = append(in.Sessions, event.SessionCertificateFlags{ID: sess.ID, Counted: *sess.Counted, Required: *sess.Required})
	}
	rule, err := h.svc.SetCertificateRule(c.Context(), p, id, in)
	if err != nil {
		return eventError(c, err)
	}
	return c.JSON(rule)
}
