package handlers

import (
	"github.com/gofiber/fiber/v3"
	"github.com/skylab-kulubu/core-backend/internal/identity"
)

type TeamHandler struct {
	svc identity.Service
}

func NewTeamHandler(svc identity.Service) *TeamHandler {
	return &TeamHandler{svc: svc}
}

func (h *TeamHandler) List(c fiber.Ctx) error {
	teams, err := h.svc.ListPublicTeams(c.Context())
	if err != nil {
		return identityError(c, err)
	}
	return c.JSON(teams)
}

func (h *TeamHandler) Members(c fiber.Ctx) error {
	roster, err := h.svc.PublicMembers(c.Context(), c.Params("team"))
	if err != nil {
		return identityError(c, err)
	}
	return c.JSON(roster)
}

func (h *TeamHandler) Leaders(c fiber.Ctx) error {
	roster, err := h.svc.PublicLeaders(c.Context(), c.Params("team"))
	if err != nil {
		return identityError(c, err)
	}
	return c.JSON(roster)
}
