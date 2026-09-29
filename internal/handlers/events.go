package handlers

import (
	"errors"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/lifecycle"
)

type EventHandler struct {
	svc event.Service
}

func NewEventHandler(svc event.Service) *EventHandler {
	return &EventHandler{svc: svc}
}

type eventBody struct {
	ID              *uuid.UUID             `json:"id"`
	Name            string                 `json:"name"`
	Description     string                 `json:"description"`
	Location        string                 `json:"location"`
	OwnerTeam       string                 `json:"ownerTeam"`
	FormURL         string                 `json:"formUrl"`
	FormAlias       *string                `json:"formAlias"`
	ExtraFormURLs   *[]event.EventFormLink `json:"extraFormUrls"`
	Capacity        int                    `json:"capacity"`
	StartDate       *time.Time             `json:"startDate"`
	EndDate         *time.Time             `json:"endDate"`
	Linkedin        string                 `json:"linkedin"`
	Active          bool                   `json:"active"`
	Ranked          bool                   `json:"ranked"`
	PrizeInfo       string                 `json:"prizeInfo"`
	CoverImageID    *uuid.UUID             `json:"coverImageId"`
	AttendanceRule  string                 `json:"attendanceRule"`
	AttendanceRatio *float64               `json:"attendanceRatio"`
	DoorStaffIDs    *[]uuid.UUID           `json:"doorStaffIds"`
}

func eventError(c fiber.Ctx, err error) error {
	if handled, problemErr := linkProblem(c, err); handled {
		return problemErr
	}
	switch {
	case errors.Is(err, fiber.ErrUnauthorized):
		return problem(c, fiber.StatusUnauthorized, "Unauthorized")
	case errors.Is(err, event.ErrForbidden):
		return problem(c, fiber.StatusForbidden, "Forbidden")
	case errors.Is(err, event.ErrNotFound):
		return problem(c, fiber.StatusNotFound, "Not Found")
	case errors.Is(err, event.ErrConflict):
		return problem(c, fiber.StatusConflict, "Conflict")
	case errors.Is(err, event.ErrInvalid):
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	default:
		return err
	}
}

func (b eventBody) asEvent() event.Event {
	e := event.Event{
		Name:            b.Name,
		Description:     b.Description,
		Location:        b.Location,
		OwnerTeam:       b.OwnerTeam,
		FormURL:         b.FormURL,
		Capacity:        b.Capacity,
		StartDate:       b.StartDate,
		EndDate:         b.EndDate,
		Linkedin:        b.Linkedin,
		Active:          b.Active,
		Ranked:          b.Ranked,
		PrizeInfo:       b.PrizeInfo,
		CoverImageID:    b.CoverImageID,
		AttendanceRule:  b.AttendanceRule,
		AttendanceRatio: b.AttendanceRatio,
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

func (h *EventHandler) List(c fiber.Ctx) error {
	visibility, err := lifecycleVisibility(c)
	if err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	owner := c.Query("ownerTeam")
	activeOnly := c.Query("active") == "true"
	p, callerErr := caller(c)
	if visibility != lifecycle.CurrentOnly {
		if callerErr != nil {
			return eventError(c, callerErr)
		}
		events, err := h.svc.ListLifecycle(c.Context(), p, owner, visibility)
		if err != nil {
			return eventError(c, err)
		}
		return c.JSON(h.svc.ProjectAllFor(&p, events))
	}
	if callerErr != nil {
		activeOnly = true
	}
	events, err := h.svc.List(c.Context(), owner, activeOnly)
	if err != nil {
		return eventError(c, err)
	}
	if callerErr != nil {
		return c.JSON(h.svc.ProjectAllFor(nil, events))
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

func (h *EventHandler) ListActive(c fiber.Ctx) error {
	events, err := h.svc.List(c.Context(), "", true)
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
	ev, err := h.svc.Get(c.Context(), id)
	if err != nil {
		return eventError(c, err)
	}
	p, callerErr := caller(c)
	if callerErr != nil {
		return c.JSON(h.svc.ProjectFor(nil, ev))
	}
	return c.JSON(h.svc.ProjectFor(&p, ev))
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
	var body eventBody
	if err := c.Bind().Body(&body); err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	updated, err := h.svc.Update(c.Context(), p, id, body.asEvent())
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
		if err := c.Bind().Body(&body); err != nil || body.PosterID == nil || *body.PosterID == uuid.Nil {
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
