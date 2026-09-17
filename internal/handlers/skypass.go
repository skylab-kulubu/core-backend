package handlers

import (
	"errors"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/skypass"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
)

type SkyPassHandler struct {
	svc     skypass.Service
	tickets ticket.Service
}

func NewSkyPassHandler(svc skypass.Service, tickets ticket.Service) *SkyPassHandler {
	return &SkyPassHandler{svc: svc, tickets: tickets}
}

type cardBindBody struct {
	UID    string     `json:"uid"`
	UserID *uuid.UUID `json:"userId"`
}

type verifyBody struct {
	Token string `json:"token"`
}

type settleBody struct {
	Token string `json:"token"`
	UID   string `json:"uid"`
}

func skypassError(c fiber.Ctx, err error) error {
	switch {
	case errors.Is(err, fiber.ErrUnauthorized):
		return problem(c, fiber.StatusUnauthorized, "Unauthorized")
	case errors.Is(err, skypass.ErrForbidden):
		return problem(c, fiber.StatusForbidden, "Forbidden")
	case errors.Is(err, skypass.ErrNotFound):
		return problem(c, fiber.StatusNotFound, "Not Found")
	case errors.Is(err, skypass.ErrConflict):
		return problem(c, fiber.StatusConflict, "Conflict")
	case errors.Is(err, skypass.ErrExpired):
		return problem(c, fiber.StatusUnauthorized, "Unauthorized")
	case errors.Is(err, skypass.ErrInvalid):
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	default:
		return err
	}
}

func (h *SkyPassHandler) JWKS(c fiber.Ctx) error {
	return c.JSON(h.svc.JWKS())
}

func (h *SkyPassHandler) BindCard(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return skypassError(c, err)
	}
	var body cardBindBody
	if err := c.Bind().Body(&body); err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	got, err := h.svc.BindCard(c.Context(), p, body.UID, body.UserID)
	if err != nil {
		return skypassError(c, err)
	}
	return c.JSON(got)
}

func (h *SkyPassHandler) LookupCard(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return skypassError(c, err)
	}
	got, err := h.svc.Lookup(c.Context(), p, c.Query("uid"))
	if err != nil {
		return skypassError(c, err)
	}
	return c.JSON(got)
}

func (h *SkyPassHandler) Mint(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return skypassError(c, err)
	}
	tok, err := h.svc.Mint(c.Context(), p)
	if err != nil {
		return skypassError(c, err)
	}
	return c.JSON(tok)
}

func (h *SkyPassHandler) Verify(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return skypassError(c, err)
	}
	var body verifyBody
	if err := c.Bind().Body(&body); err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	got, err := h.svc.Verify(c.Context(), p, body.Token)
	if err != nil {
		return skypassError(c, err)
	}
	return c.JSON(got)
}

func (h *SkyPassHandler) CheckInSession(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return skypassError(c, err)
	}
	sessionID, err := uuid.Parse(c.Params("sessionId"))
	if err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	var body settleBody
	if err := c.Bind().Body(&body); err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	got, err := h.svc.HolderFrom(c.Context(), body.Token, body.UID)
	if err != nil {
		return skypassError(c, err)
	}
	if h.tickets == nil {
		return problem(c, fiber.StatusInternalServerError, "Internal Server Error")
	}
	ci, err := h.tickets.CheckInUser(c.Context(), p, sessionID, got.ID)
	if err != nil {
		return ticketError(c, err)
	}
	return c.Status(fiber.StatusCreated).JSON(ci)
}
