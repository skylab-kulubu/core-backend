package handlers

import (
	"errors"
	"strings"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/competition"
	"github.com/skylab-kulubu/core-backend/internal/competition/scoring"
)

// CompetitionHandler serves the organizers' competition routes
// (docs/competitions.md).
type CompetitionHandler struct {
	svc competition.Service
}

func NewCompetitionHandler(svc competition.Service) *CompetitionHandler {
	return &CompetitionHandler{svc: svc}
}

// competitionError answers the service's errors. A 400 or 409 carries a
// code the panel can branch on.
func competitionError(c fiber.Ctx, err error) error {
	switch {
	case errors.Is(err, fiber.ErrUnauthorized):
		return problem(c, fiber.StatusUnauthorized, "Unauthorized")
	case errors.Is(err, competition.ErrForbidden):
		return problem(c, fiber.StatusForbidden, "Forbidden")
	case errors.Is(err, competition.ErrNotFound):
		return problem(c, fiber.StatusNotFound, "Not Found")
	case errors.Is(err, competition.ErrStatus):
		return problemDetailCode(c, fiber.StatusConflict, "Conflict", "the competition's status does not allow this", "competition_status")
	case errors.Is(err, competition.ErrTicketTaken):
		return problemDetailCode(c, fiber.StatusConflict, "Conflict", "a ticket already is an entrant of this competition", "ticket_taken")
	case errors.Is(err, competition.ErrConflict):
		return problem(c, fiber.StatusConflict, "Conflict")
	case errors.Is(err, competition.ErrReasonRequired):
		return problemDetailCode(c, fiber.StatusBadRequest, "Bad Request", "a reason is required", "reason_required")
	case errors.Is(err, competition.ErrInvalid):
		return problemDetail(c, fiber.StatusBadRequest, "Bad Request", strings.TrimPrefix(err.Error(), "competition: invalid: "))
	default:
		return err
	}
}

func badRequest(c fiber.Ctx) error {
	return problem(c, fiber.StatusBadRequest, "Bad Request")
}

func pathID(c fiber.Ctx, name string) (uuid.UUID, bool) {
	id, err := uuid.Parse(c.Params(name))
	return id, err == nil
}

type competitionCreateBody struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	EntrantMode string `json:"entrantMode"`
}

func (h *CompetitionHandler) Create(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return competitionError(c, err)
	}
	eventID, ok := pathID(c, "eventId")
	if !ok {
		return badRequest(c)
	}
	var body competitionCreateBody
	if err := c.Bind().Body(&body); err != nil {
		return badRequest(c)
	}
	created, err := h.svc.Create(c.Context(), p, eventID, competition.CreateInput{
		Name: body.Name, Description: body.Description, EntrantMode: body.EntrantMode,
	})
	if err != nil {
		return competitionError(c, err)
	}
	return c.Status(fiber.StatusCreated).JSON(created)
}

func (h *CompetitionHandler) ListByEvent(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return competitionError(c, err)
	}
	eventID, ok := pathID(c, "eventId")
	if !ok {
		return badRequest(c)
	}
	visibility, err := lifecycleVisibility(c)
	if err != nil {
		return badRequest(c)
	}
	list, err := h.svc.ListByEvent(c.Context(), p, eventID, visibility)
	if err != nil {
		return competitionError(c, err)
	}
	return c.JSON(list)
}

// withCompetition runs do for the caller and the :id competition.
func (h *CompetitionHandler) withCompetition(c fiber.Ctx, do func(p authz.Principal, id uuid.UUID) (any, int, error)) error {
	p, err := caller(c)
	if err != nil {
		return competitionError(c, err)
	}
	id, ok := pathID(c, "id")
	if !ok {
		return badRequest(c)
	}
	out, status, err := do(p, id)
	if err != nil {
		return competitionError(c, err)
	}
	if out == nil {
		return c.SendStatus(status)
	}
	return c.Status(status).JSON(out)
}

func (h *CompetitionHandler) Get(c fiber.Ctx) error {
	return h.withCompetition(c, func(p authz.Principal, id uuid.UUID) (any, int, error) {
		got, err := h.svc.Get(c.Context(), p, id)
		return got, fiber.StatusOK, err
	})
}

type competitionUpdateBody struct {
	Name        *string `json:"name"`
	Description *string `json:"description"`
}

func (h *CompetitionHandler) Update(c fiber.Ctx) error {
	var body competitionUpdateBody
	if err := c.Bind().Body(&body); err != nil {
		return badRequest(c)
	}
	return h.withCompetition(c, func(p authz.Principal, id uuid.UUID) (any, int, error) {
		got, err := h.svc.Update(c.Context(), p, id, competition.UpdateInput{Name: body.Name, Description: body.Description})
		return got, fiber.StatusOK, err
	})
}

type reasonBody struct {
	Reason string `json:"reason"`
}

// Transition answers POST /v1/competitions/:id/{open,start,close,reopen,cancel}.
func (h *CompetitionHandler) Transition(action string) fiber.Handler {
	return func(c fiber.Ctx) error {
		var body reasonBody
		if action == "cancel" {
			if err := c.Bind().Body(&body); err != nil {
				return badRequest(c)
			}
		}
		return h.withCompetition(c, func(p authz.Principal, id uuid.UUID) (any, int, error) {
			var got competition.Competition
			var err error
			ctx := c.Context()
			switch action {
			case "open":
				got, err = h.svc.Open(ctx, p, id)
			case "start":
				got, err = h.svc.Start(ctx, p, id)
			case "close":
				got, err = h.svc.Close(ctx, p, id)
			case "reopen":
				got, err = h.svc.Reopen(ctx, p, id)
			case "cancel":
				got, err = h.svc.Cancel(ctx, p, id, body.Reason)
			default:
				return nil, 0, competition.ErrNotFound
			}
			return got, fiber.StatusOK, err
		})
	}
}

func (h *CompetitionHandler) Archive(c fiber.Ctx) error {
	return h.withCompetition(c, func(p authz.Principal, id uuid.UUID) (any, int, error) {
		return nil, fiber.StatusNoContent, h.svc.Archive(c.Context(), p, id)
	})
}

func (h *CompetitionHandler) Restore(c fiber.Ctx) error {
	return h.withCompetition(c, func(p authz.Principal, id uuid.UUID) (any, int, error) {
		got, err := h.svc.Restore(c.Context(), p, id)
		return got, fiber.StatusOK, err
	})
}

type entrantsBody struct {
	TicketIDs []uuid.UUID `json:"ticketIds"`
}

func (h *CompetitionHandler) AddEntrants(c fiber.Ctx) error {
	var body entrantsBody
	if err := c.Bind().Body(&body); err != nil {
		return badRequest(c)
	}
	return h.withCompetition(c, func(p authz.Principal, id uuid.UUID) (any, int, error) {
		added, err := h.svc.AddEntrants(c.Context(), p, id, body.TicketIDs)
		return added, fiber.StatusCreated, err
	})
}

func (h *CompetitionHandler) Entrants(c fiber.Ctx) error {
	return h.withCompetition(c, func(p authz.Principal, id uuid.UUID) (any, int, error) {
		list, err := h.svc.Entrants(c.Context(), p, id)
		return list, fiber.StatusOK, err
	})
}

type scoreBody struct {
	EntrantID uuid.UUID       `json:"entrantId"`
	StageID   *uuid.UUID      `json:"stageId"`
	Points    *scoring.Points `json:"points"`
	Reason    string          `json:"reason"`
}

func (h *CompetitionHandler) SetScore(c fiber.Ctx) error {
	var body scoreBody
	if err := c.Bind().Body(&body); err != nil {
		return problemDetailCode(c, fiber.StatusBadRequest, "Bad Request", scoring.ErrInvalidPoints.Error(), "invalid_points")
	}
	if body.Points == nil || body.EntrantID == uuid.Nil {
		return problemDetail(c, fiber.StatusBadRequest, "Bad Request", "entrantId and points are required")
	}
	return h.withCompetition(c, func(p authz.Principal, id uuid.UUID) (any, int, error) {
		entry, err := h.svc.SetScore(c.Context(), p, id, competition.ScoreInput{
			EntrantID: body.EntrantID, StageID: body.StageID, Points: *body.Points, Reason: body.Reason,
		})
		return entry, fiber.StatusCreated, err
	})
}

func (h *CompetitionHandler) RevokeEntry(c fiber.Ctx) error {
	var body reasonBody
	if err := c.Bind().Body(&body); err != nil {
		return badRequest(c)
	}
	entryID, ok := pathID(c, "entryId")
	if !ok {
		return badRequest(c)
	}
	return h.withCompetition(c, func(p authz.Principal, id uuid.UUID) (any, int, error) {
		entry, err := h.svc.Revoke(c.Context(), p, id, entryID, body.Reason)
		return entry, fiber.StatusOK, err
	})
}

func (h *CompetitionHandler) Entries(c fiber.Ctx) error {
	return h.withCompetition(c, func(p authz.Principal, id uuid.UUID) (any, int, error) {
		list, err := h.svc.Entries(c.Context(), p, id)
		return list, fiber.StatusOK, err
	})
}

func (h *CompetitionHandler) Leaderboard(c fiber.Ctx) error {
	return h.withCompetition(c, func(p authz.Principal, id uuid.UUID) (any, int, error) {
		board, err := h.svc.Leaderboard(c.Context(), p, id)
		return board, fiber.StatusOK, err
	})
}
