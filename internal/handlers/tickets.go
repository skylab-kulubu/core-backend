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

func (h *TicketHandler) ApplyForOther(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return ticketError(c, err)
	}
	eventID, err := uuid.Parse(c.Params("eventId"))
	if err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	userID, err := uuid.Parse(c.Params("userId"))
	if err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	created, err := h.svc.ApplyForOther(c.Context(), p, eventID, userID)
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

func (h *TicketHandler) ListByEvent(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return ticketError(c, err)
	}
	eventID, err := uuid.Parse(c.Params("eventId"))
	if err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	tickets, err := h.svc.ListByEvent(c.Context(), p, eventID)
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
	sessionID, err := uuid.Parse(c.Params("sessionId"))
	if err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	ci, err := h.svc.CheckIn(c.Context(), p, ticketID, sessionID)
	if err != nil {
		return ticketError(c, err)
	}
	return c.Status(fiber.StatusCreated).JSON(ci)
}

type guestCheckInBody struct {
	Email string `json:"email"`
}

func (h *TicketHandler) CheckInMe(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return ticketError(c, err)
	}
	sessionID, err := uuid.Parse(c.Params("sessionId"))
	if err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	ci, err := h.svc.CheckInMe(c.Context(), p, sessionID)
	if err != nil {
		return ticketError(c, err)
	}
	return c.Status(fiber.StatusCreated).JSON(ci)
}

func (h *TicketHandler) CheckInGuest(c fiber.Ctx) error {
	sessionID, err := uuid.Parse(c.Params("sessionId"))
	if err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	var body guestCheckInBody
	if err := c.Bind().Body(&body); err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	ci, err := h.svc.CheckInGuest(c.Context(), sessionID, body.Email)
	if err != nil {
		return ticketError(c, err)
	}
	return c.Status(fiber.StatusCreated).JSON(ci)
}

func (h *TicketHandler) Get(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return ticketError(c, err)
	}
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	got, err := h.svc.Get(c.Context(), p, id)
	if err != nil {
		return ticketError(c, err)
	}
	return c.JSON(got)
}

func (h *TicketHandler) ByUserEvent(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return ticketError(c, err)
	}
	userID, err := uuid.Parse(c.Params("userId"))
	if err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	eventID, err := uuid.Parse(c.Params("eventId"))
	if err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	got, err := h.svc.GetByUserEvent(c.Context(), p, userID, eventID)
	if err != nil {
		return ticketError(c, err)
	}
	return c.JSON(got)
}

func (h *TicketHandler) List(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return ticketError(c, err)
	}
	email := c.Query("email")
	var userID *uuid.UUID
	if raw := c.Query("userId"); raw != "" {
		id, err := uuid.Parse(raw)
		if err != nil {
			return problem(c, fiber.StatusBadRequest, "Bad Request")
		}
		userID = &id
	}
	tickets, err := h.svc.ListQuery(c.Context(), p, email, userID)
	if err != nil {
		return ticketError(c, err)
	}
	return c.JSON(tickets)
}
