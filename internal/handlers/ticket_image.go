package handlers

import (
	"errors"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/ticketimage"
)

// TicketImageHandler answers /v1/events/{id}/ticket-image
// (docs/ticket-image.md): the image the PDF ticket shows in its band.
type TicketImageHandler struct {
	svc *ticketimage.Service
}

func NewTicketImageHandler(svc *ticketimage.Service) *TicketImageHandler {
	return &TicketImageHandler{svc: svc}
}

func ticketImageError(c fiber.Ctx, err error) error {
	if errors.Is(err, ticketimage.ErrInvalid) {
		return problemCode(c, fiber.StatusBadRequest, "Bad Request", "ticket_image_invalid")
	}
	return eventError(c, err)
}

// Get is GET /v1/events/{id}/ticket-image.
func (h *TicketImageHandler) Get(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return ticketImageError(c, err)
	}
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	view, err := h.svc.Get(c.Context(), p, id)
	if err != nil {
		return ticketImageError(c, err)
	}
	return c.JSON(view)
}

// Put is PUT /v1/events/{id}/ticket-image with {source, mediaId,
// coverMediaId, crop: {x, y, w, h}}.
func (h *TicketImageHandler) Put(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return ticketImageError(c, err)
	}
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	var body ticketimage.Input
	if err := c.Bind().Body(&body); err != nil {
		return ticketImageError(c, ticketimage.ErrInvalid)
	}
	view, err := h.svc.Set(c.Context(), p, id, body)
	if err != nil {
		return ticketImageError(c, err)
	}
	return c.JSON(view)
}
