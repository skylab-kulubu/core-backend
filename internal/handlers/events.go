package handlers

import (
	"errors"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/event"
)

type EventHandler struct {
	svc event.Service
}

func NewEventHandler(svc event.Service) *EventHandler {
	return &EventHandler{svc: svc}
}

type eventBody struct {
	Name            string     `json:"name"`
	Description     string     `json:"description"`
	Location        string     `json:"location"`
	OwnerTeam       string     `json:"ownerTeam"`
	FormURL         string     `json:"formUrl"`
	Capacity        int        `json:"capacity"`
	StartDate       *time.Time `json:"startDate"`
	EndDate         *time.Time `json:"endDate"`
	Linkedin        string     `json:"linkedin"`
	Active          bool       `json:"active"`
	Ranked          bool       `json:"ranked"`
	PrizeInfo       string     `json:"prizeInfo"`
	CoverImageID    *uuid.UUID `json:"coverImageId"`
	AttendanceRule  string     `json:"attendanceRule"`
	AttendanceRatio *float64   `json:"attendanceRatio"`
}

func eventError(c fiber.Ctx, err error) error {
	switch {
	case errors.Is(err, fiber.ErrUnauthorized):
		return problem(c, fiber.StatusUnauthorized, "Unauthorized")
	case errors.Is(err, event.ErrForbidden):
		return problem(c, fiber.StatusForbidden, "Forbidden")
	case errors.Is(err, event.ErrNotFound):
		return problem(c, fiber.StatusNotFound, "Not Found")
	case errors.Is(err, event.ErrInvalid):
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	default:
		return err
	}
}

func (b eventBody) asEvent() event.Event {
	return event.Event{
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
}

func (h *EventHandler) List(c fiber.Ctx) error {
	owner := c.Query("ownerTeam")
	activeOnly := c.Query("active") == "true"
	if _, err := caller(c); err != nil {
		activeOnly = true
	}
	events, err := h.svc.List(c.Context(), owner, activeOnly)
	if err != nil {
		return eventError(c, err)
	}
	return c.JSON(events)
}

func (h *EventHandler) ListActive(c fiber.Ctx) error {
	events, err := h.svc.List(c.Context(), "", true)
	if err != nil {
		return eventError(c, err)
	}
	return c.JSON(events)
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
	return c.JSON(ev)
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
	return c.Status(fiber.StatusCreated).JSON(created)
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
	return c.JSON(updated)
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
	return c.JSON(updated)
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
	return c.JSON(updated)
}
