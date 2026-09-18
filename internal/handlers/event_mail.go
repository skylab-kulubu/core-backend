package handlers

import (
	"errors"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/eventmail"
	"github.com/skylab-kulubu/core-backend/internal/mail"
)

type EventMailHandler struct {
	svc eventmail.Service
}

func NewEventMailHandler(svc eventmail.Service) *EventMailHandler {
	return &EventMailHandler{svc: svc}
}

func (h *EventMailHandler) Sync(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return eventMailError(c, err)
	}
	eventID, err := uuid.Parse(c.Params("eventId"))
	if err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	result, err := h.svc.Sync(c.Context(), p, eventID)
	if err != nil {
		return eventMailError(c, err)
	}
	return c.JSON(result)
}

func eventMailError(c fiber.Ctx, err error) error {
	switch {
	case errors.Is(err, fiber.ErrUnauthorized):
		return problem(c, fiber.StatusUnauthorized, "Unauthorized")
	case errors.Is(err, eventmail.ErrForbidden):
		return problem(c, fiber.StatusForbidden, "Forbidden")
	case errors.Is(err, eventmail.ErrNotFound):
		return problem(c, fiber.StatusNotFound, "Not Found")
	case errors.Is(err, eventmail.ErrUnavailable):
		return problem(c, fiber.StatusServiceUnavailable, "Skymail is not configured")
	case errors.Is(err, mail.ErrForbidden):
		return problemDetail(c, fiber.StatusForbidden, "Forbidden", mail.ErrForbidden.Error()+" / Skymail listesi yasak — core servis hesabına skymail:lists:write ver")
	default:
		var status *mail.StatusError
		if errors.As(err, &status) {
			return problemDetail(c, fiber.StatusBadGateway, "Bad Gateway", status.Error())
		}
		return err
	}
}
