package handlers

import (
	"errors"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/season"
)

type SeasonHandler struct {
	seasons season.Service
	events  event.Service
}

func NewSeasonHandler(seasons season.Service, events event.Service) *SeasonHandler {
	return &SeasonHandler{seasons: seasons, events: events}
}

type seasonBody struct {
	Name      string     `json:"name"`
	StartDate *time.Time `json:"startDate"`
	EndDate   *time.Time `json:"endDate"`
	Active    bool       `json:"active"`
}

func seasonError(c fiber.Ctx, err error) error {
	switch {
	case errors.Is(err, fiber.ErrUnauthorized):
		return problem(c, fiber.StatusUnauthorized, "Unauthorized")
	case errors.Is(err, season.ErrForbidden), errors.Is(err, event.ErrForbidden):
		return problem(c, fiber.StatusForbidden, "Forbidden")
	case errors.Is(err, season.ErrNotFound), errors.Is(err, event.ErrNotFound):
		return problem(c, fiber.StatusNotFound, "Not Found")
	case errors.Is(err, season.ErrInvalid), errors.Is(err, event.ErrInvalid):
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	default:
		return err
	}
}

func (h *SeasonHandler) List(c fiber.Ctx) error {
	activeOnly := c.Query("active") == "true"
	items, err := h.seasons.List(c.Context(), activeOnly)
	if err != nil {
		return seasonError(c, err)
	}
	return c.JSON(items)
}

func (h *SeasonHandler) Get(c fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	item, err := h.seasons.Get(c.Context(), id)
	if err != nil {
		return seasonError(c, err)
	}
	return c.JSON(item)
}

func (h *SeasonHandler) Create(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return seasonError(c, err)
	}
	var body seasonBody
	if err := c.Bind().Body(&body); err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	created, err := h.seasons.Create(c.Context(), p, season.Season{
		Name: body.Name, StartDate: body.StartDate, EndDate: body.EndDate, Active: body.Active,
	})
	if err != nil {
		return seasonError(c, err)
	}
	return c.Status(fiber.StatusCreated).JSON(created)
}

func (h *SeasonHandler) Update(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return seasonError(c, err)
	}
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	var body seasonBody
	if err := c.Bind().Body(&body); err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	updated, err := h.seasons.Update(c.Context(), p, id, season.Season{
		Name: body.Name, StartDate: body.StartDate, EndDate: body.EndDate, Active: body.Active,
	})
	if err != nil {
		return seasonError(c, err)
	}
	return c.JSON(updated)
}

func (h *SeasonHandler) Delete(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return seasonError(c, err)
	}
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	if err := h.seasons.Delete(c.Context(), p, id); err != nil {
		return seasonError(c, err)
	}
	return c.SendStatus(fiber.StatusNoContent)
}

func (h *SeasonHandler) ListEvents(c fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	if _, err := h.seasons.Get(c.Context(), id); err != nil {
		return seasonError(c, err)
	}
	events, err := h.events.ListBySeason(c.Context(), id)
	if err != nil {
		return seasonError(c, err)
	}
	return c.JSON(events)
}

func (h *SeasonHandler) AssignEvent(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return seasonError(c, err)
	}
	seasonID, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	eventID, err := uuid.Parse(c.Params("eventId"))
	if err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	if _, err := h.seasons.Get(c.Context(), seasonID); err != nil {
		return seasonError(c, err)
	}
	updated, err := h.events.AssignSeason(c.Context(), p, eventID, &seasonID)
	if err != nil {
		return seasonError(c, err)
	}
	return c.JSON(updated)
}

func (h *SeasonHandler) UnassignEvent(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return seasonError(c, err)
	}
	eventID, err := uuid.Parse(c.Params("eventId"))
	if err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	updated, err := h.events.AssignSeason(c.Context(), p, eventID, nil)
	if err != nil {
		return seasonError(c, err)
	}
	return c.JSON(updated)
}
