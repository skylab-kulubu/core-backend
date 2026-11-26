package handlers

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/raffle"
	"github.com/skylab-kulubu/core-backend/internal/teamaudit"
)

// Raffles is an Event's raffles (raffle.Service).
type Raffles interface {
	Preview(ctx context.Context, p authz.Principal, eventID uuid.UUID, c raffle.Criteria) (raffle.Preview, error)
	Draw(ctx context.Context, p authz.Principal, eventID uuid.UUID, req raffle.Request) (raffle.Raffle, error)
	List(ctx context.Context, p authz.Principal, eventID uuid.UUID) ([]raffle.Summary, error)
	Get(ctx context.Context, p authz.Principal, eventID, raffleID uuid.UUID) (raffle.Raffle, error)
	Verification(ctx context.Context, p authz.Principal, eventID, raffleID uuid.UUID) (raffle.Verification, error)
	Forfeit(ctx context.Context, p authz.Principal, eventID, raffleID uuid.UUID, position int, reason string) (raffle.Raffle, error)
}

// RaffleHandler serves /v1/events/{id}/raffles (docs/raffle.md).
type RaffleHandler struct {
	raffles Raffles
}

// NewRaffleHandler builds the handler.
func NewRaffleHandler(raffles Raffles) *RaffleHandler {
	return &RaffleHandler{raffles: raffles}
}

// event reads the caller and the Event's id; with ok false the answer is
// written (401, 400 invalid_id) and err is what writing it returned.
func (h *RaffleHandler) event(c fiber.Ctx) (p authz.Principal, eventID uuid.UUID, ok bool, err error) {
	p, err = caller(c)
	if err != nil {
		return p, uuid.Nil, false, problem(c, fiber.StatusUnauthorized, "Unauthorized")
	}
	eventID, err = uuid.Parse(c.Params("id"))
	if err != nil {
		return p, uuid.Nil, false, badID(c, "id")
	}
	return p, eventID, true, nil
}

// raffle is event and the raffle's id.
func (h *RaffleHandler) raffle(c fiber.Ctx) (p authz.Principal, eventID, raffleID uuid.UUID, ok bool, err error) {
	p, eventID, ok, err = h.event(c)
	if !ok {
		return p, eventID, uuid.Nil, false, err
	}
	raffleID, err = uuid.Parse(c.Params("raffleId"))
	if err != nil {
		return p, eventID, uuid.Nil, false, badID(c, "raffleId")
	}
	return p, eventID, raffleID, true, nil
}

// Preview is POST /v1/events/{id}/raffles/preview {criteria}: 200 {eligible,
// excluded, candidates (masked names), truncated}.
func (h *RaffleHandler) Preview(c fiber.Ctx) error {
	p, eventID, ok, err := h.event(c)
	if !ok {
		return err
	}
	var body struct {
		Criteria raffle.Criteria `json:"criteria"`
	}
	if err := json.Unmarshal(c.Body(), &body); err != nil {
		return badBody(c)
	}
	preview, err := h.raffles.Preview(c.Context(), p, eventID, body.Criteria)
	if err != nil {
		return raffleError(c, err)
	}
	c.Set(fiber.HeaderCacheControl, "no-store")
	return c.JSON(preview)
}

// Draw is POST /v1/events/{id}/raffles {title, criteria, winners,
// alternates}: 201 the raffle. Its Team audit row names the caller
// (auditContext): without them it is refused 503 and nothing is drawn.
func (h *RaffleHandler) Draw(c fiber.Ctx) error {
	p, eventID, ok, err := h.event(c)
	if !ok {
		return err
	}
	var req raffle.Request
	if err := json.Unmarshal(c.Body(), &req); err != nil {
		return badBody(c)
	}
	drawn, err := h.raffles.Draw(auditContext(c, p), p, eventID, req)
	if err != nil {
		return raffleError(c, err)
	}
	c.Set(fiber.HeaderCacheControl, "no-store")
	return c.Status(fiber.StatusCreated).JSON(drawn)
}

// List is GET /v1/events/{id}/raffles: 200 {items}, newest first.
func (h *RaffleHandler) List(c fiber.Ctx) error {
	p, eventID, ok, err := h.event(c)
	if !ok {
		return err
	}
	items, err := h.raffles.List(c.Context(), p, eventID)
	if err != nil {
		return raffleError(c, err)
	}
	c.Set(fiber.HeaderCacheControl, "no-store")
	return c.JSON(fiber.Map{"items": items})
}

// Get is GET /v1/events/{id}/raffles/{raffleId}: 200 the raffle.
func (h *RaffleHandler) Get(c fiber.Ctx) error {
	p, eventID, raffleID, ok, err := h.raffle(c)
	if !ok {
		return err
	}
	r, err := h.raffles.Get(c.Context(), p, eventID, raffleID)
	if err != nil {
		return raffleError(c, err)
	}
	c.Set(fiber.HeaderCacheControl, "no-store")
	return c.JSON(r)
}

// Verification is GET /v1/events/{id}/raffles/{raffleId}/verification: 200
// {algorithm, seed, listHash, ticketIds, order}.
func (h *RaffleHandler) Verification(c fiber.Ctx) error {
	p, eventID, raffleID, ok, err := h.raffle(c)
	if !ok {
		return err
	}
	v, err := h.raffles.Verification(c.Context(), p, eventID, raffleID)
	if err != nil {
		return raffleError(c, err)
	}
	c.Set(fiber.HeaderCacheControl, "no-store")
	return c.JSON(v)
}

// Forfeit is POST /v1/events/{id}/raffles/{raffleId}/forfeits {position,
// reason}: 200 the raffle, its next alternate a winner. Audited as Draw.
func (h *RaffleHandler) Forfeit(c fiber.Ctx) error {
	p, eventID, raffleID, ok, err := h.raffle(c)
	if !ok {
		return err
	}
	var body struct {
		Position int    `json:"position"`
		Reason   string `json:"reason"`
	}
	if err := json.Unmarshal(c.Body(), &body); err != nil {
		return badBody(c)
	}
	r, err := h.raffles.Forfeit(auditContext(c, p), p, eventID, raffleID, body.Position, body.Reason)
	if err != nil {
		return raffleError(c, err)
	}
	c.Set(fiber.HeaderCacheControl, "no-store")
	return c.JSON(r)
}

func raffleError(c fiber.Ctx, err error) error {
	var tooFew *raffle.TooFewError
	switch {
	case errors.As(err, &tooFew):
		return problemWithFields(c, fiber.StatusConflict, "Conflict", "Conflict", "raffle_too_few_candidates",
			fiber.Map{"eligible": tooFew.Eligible})
	case errors.Is(err, raffle.ErrInvalid):
		return problemCode(c, fiber.StatusBadRequest, "Bad Request", "raffle_invalid")
	case errors.Is(err, raffle.ErrEventNotFound):
		return problem(c, fiber.StatusNotFound, "Not Found")
	case errors.Is(err, raffle.ErrNotFound):
		return problemCode(c, fiber.StatusNotFound, "Not Found", "raffle_not_found")
	case errors.Is(err, raffle.ErrForbidden):
		return problem(c, fiber.StatusForbidden, "Forbidden")
	case errors.Is(err, raffle.ErrNotWinner):
		return problemCode(c, fiber.StatusConflict, "Conflict", "raffle_entry_not_winner")
	case errors.Is(err, raffle.ErrNoAlternates):
		return problemCode(c, fiber.StatusConflict, "Conflict", "raffle_no_alternates")
	case errors.Is(err, raffle.ErrDirectory):
		c.Set(fiber.HeaderRetryAfter, "5")
		return problemCode(c, fiber.StatusServiceUnavailable, "Service Unavailable", "raffle_directory_unavailable")
	case errors.Is(err, teamaudit.ErrUnavailable):
		c.Set(fiber.HeaderCacheControl, "no-store")
		return problemCode(c, fiber.StatusServiceUnavailable, "Service Unavailable", auditUnavailableCode)
	default:
		return err
	}
}
