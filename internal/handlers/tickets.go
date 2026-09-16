package handlers

import (
	"errors"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
)

type TicketHandler struct {
	svc ticket.Service
}

func NewTicketHandler(svc ticket.Service) *TicketHandler {
	return &TicketHandler{svc: svc}
}

type guestBody struct {
	FirstName   string `json:"firstName"`
	LastName    string `json:"lastName"`
	Email       string `json:"email"`
	PhoneNumber string `json:"phoneNumber"`
}

func ticketError(c fiber.Ctx, err error) error {
	switch {
	case errors.Is(err, fiber.ErrUnauthorized):
		return problem(c, fiber.StatusUnauthorized, "Unauthorized")
	case errors.Is(err, ticket.ErrForbidden):
		return problem(c, fiber.StatusForbidden, "Forbidden")
	case errors.Is(err, ticket.ErrNotFound):
		return problem(c, fiber.StatusNotFound, "Not Found")
	case errors.Is(err, ticket.ErrConflict):
		return problem(c, fiber.StatusConflict, "Conflict")
	case errors.Is(err, ticket.ErrInvalid):
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	default:
		return err
	}
}

func (h *TicketHandler) Apply(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return ticketError(c, err)
	}
	eventID, err := uuid.Parse(c.Params("eventId"))
	if err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	created, err := h.svc.Apply(c.Context(), p, eventID)
	if err != nil {
		return ticketError(c, err)
	}
	return c.Status(fiber.StatusCreated).JSON(created)
}

func (h *TicketHandler) ApplyGuest(c fiber.Ctx) error {
	eventID, err := uuid.Parse(c.Params("eventId"))
	if err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	var body guestBody
	if err := c.Bind().Body(&body); err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	created, err := h.svc.ApplyGuest(c.Context(), eventID, ticket.GuestInfo{
		FirstName: body.FirstName, LastName: body.LastName, Email: body.Email, PhoneNumber: body.PhoneNumber,
	})
	if err != nil {
		return ticketError(c, err)
	}
	return c.Status(fiber.StatusCreated).JSON(created)
}

func (h *TicketHandler) Mine(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return ticketError(c, err)
	}
	tickets, err := h.svc.Mine(c.Context(), p)
	if err != nil {
		return ticketError(c, err)
	}
	return c.JSON(tickets)
}

func (h *TicketHandler) CheckIn(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return ticketError(c, err)
	}
	ticketID, err := uuid.Parse(c.Params("ticketId"))
	if err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	dayID, err := uuid.Parse(c.Params("eventDayId"))
	if err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	ci, err := h.svc.CheckIn(c.Context(), p, ticketID, dayID)
	if err != nil {
		return ticketError(c, err)
	}
	return c.Status(fiber.StatusCreated).JSON(ci)
}
