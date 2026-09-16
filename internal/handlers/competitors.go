package handlers

import (
	"errors"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/competitor"
)

type CompetitorHandler struct {
	svc competitor.Service
}

func NewCompetitorHandler(svc competitor.Service) *CompetitorHandler {
	return &CompetitorHandler{svc: svc}
}

type competitorBody struct {
	UserID   uuid.UUID `json:"userId"`
	EventID  uuid.UUID `json:"eventId"`
	Score    *float64  `json:"score"`
	IsWinner bool      `json:"isWinner"`
}

func competitorError(c fiber.Ctx, err error) error {
	switch {
	case errors.Is(err, fiber.ErrUnauthorized):
		return problem(c, fiber.StatusUnauthorized, "Unauthorized")
	case errors.Is(err, competitor.ErrForbidden):
		return problem(c, fiber.StatusForbidden, "Forbidden")
	case errors.Is(err, competitor.ErrNotFound):
		return problem(c, fiber.StatusNotFound, "Not Found")
	case errors.Is(err, competitor.ErrConflict):
		return problem(c, fiber.StatusConflict, "Conflict")
	case errors.Is(err, competitor.ErrInvalid):
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	default:
		return err
	}
}

func (h *CompetitorHandler) List(c fiber.Ctx) error {
	comps, err := h.svc.List(c.Context())
	if err != nil {
		return competitorError(c, err)
	}
	return c.JSON(comps)
}

func (h *CompetitorHandler) Get(c fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	got, err := h.svc.Get(c.Context(), id)
	if err != nil {
		return competitorError(c, err)
	}
	return c.JSON(got)
}

func (h *CompetitorHandler) Create(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return competitorError(c, err)
	}
	var body competitorBody
	if err := c.Bind().Body(&body); err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	created, err := h.svc.Create(c.Context(), p, competitor.CreateInput{
		UserID: body.UserID, EventID: body.EventID, Score: body.Score, IsWinner: body.IsWinner,
	})
	if err != nil {
		return competitorError(c, err)
	}
	return c.Status(fiber.StatusCreated).JSON(created)
}

func (h *CompetitorHandler) Update(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return competitorError(c, err)
	}
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	var body competitorBody
	if err := c.Bind().Body(&body); err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	updated, err := h.svc.Update(c.Context(), p, id, competitor.UpdateInput{
		UserID: body.UserID, EventID: body.EventID, Score: body.Score, IsWinner: body.IsWinner,
	})
	if err != nil {
		return competitorError(c, err)
	}
	return c.JSON(updated)
}

func (h *CompetitorHandler) Delete(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return competitorError(c, err)
	}
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	if err := h.svc.Delete(c.Context(), p, id); err != nil {
		return competitorError(c, err)
	}
	return c.SendStatus(fiber.StatusNoContent)
}

func (h *CompetitorHandler) Mine(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return competitorError(c, err)
	}
	comps, err := h.svc.Mine(c.Context(), p)
	if err != nil {
		return competitorError(c, err)
	}
	return c.JSON(comps)
}

func (h *CompetitorHandler) ListByEvent(c fiber.Ctx) error {
	eventID, err := uuid.Parse(c.Params("eventId"))
	if err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	comps, err := h.svc.ListByEvent(c.Context(), eventID)
	if err != nil {
		return competitorError(c, err)
	}
	return c.JSON(comps)
}

func (h *CompetitorHandler) Winner(c fiber.Ctx) error {
	eventID, err := uuid.Parse(c.Params("eventId"))
	if err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	got, err := h.svc.Winner(c.Context(), eventID)
	if err != nil {
		return competitorError(c, err)
	}
	return c.JSON(got)
}

func (h *CompetitorHandler) ListByUser(c fiber.Ctx) error {
	userID, err := uuid.Parse(c.Params("userId"))
	if err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	comps, err := h.svc.ListByUser(c.Context(), userID)
	if err != nil {
		return competitorError(c, err)
	}
	return c.JSON(comps)
}

func (h *CompetitorHandler) ListByOwnerTeam(c fiber.Ctx) error {
	comps, err := h.svc.ListByOwnerTeam(c.Context(), c.Params("ownerTeam"))
	if err != nil {
		return competitorError(c, err)
	}
	return c.JSON(comps)
}

func (h *CompetitorHandler) LeaderboardByType(c fiber.Ctx) error {
	board, err := h.svc.Leaderboard(c.Context(), c.Params("eventType"), nil)
	if err != nil {
		return competitorError(c, err)
	}
	return c.JSON(board)
}

func (h *CompetitorHandler) LeaderboardBySeason(c fiber.Ctx) error {
	seasonID, err := uuid.Parse(c.Params("seasonId"))
	if err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	board, err := h.svc.Leaderboard(c.Context(), c.Params("eventType"), &seasonID)
	if err != nil {
		return competitorError(c, err)
	}
	return c.JSON(board)
}
