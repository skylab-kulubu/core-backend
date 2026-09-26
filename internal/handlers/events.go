package handlers

import (
	"errors"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
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
	updated, err := h.svc.AddImages(c.Context(), p, id, ids)
	if err != nil {
		return eventError(c, err)
	}
	return c.JSON(h.svc.ProjectFor(&p, updated))
}

func (h *EventHandler) RemoveImages(c fiber.Ctx) error {
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
	updated, err := h.svc.RemoveImages(c.Context(), p, id, ids)
	if err != nil {
		return eventError(c, err)
	}
	return c.JSON(h.svc.ProjectFor(&p, updated))
}
