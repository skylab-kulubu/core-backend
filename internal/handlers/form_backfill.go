package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"log"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/requestid"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/formsfeed"
)

// FormBackfills queues an organizer's backfill of a form
// (formsfeed.Backfills).
type FormBackfills interface {
	Request(ctx context.Context, p authz.Principal, eventID, formID uuid.UUID) (bool, error)
}

// FormBackfillHandler is the panel's "Bu kişilere de bilet yaz"
// (docs/form-pulls.md).
type FormBackfillHandler struct {
	backfills FormBackfills
	logger    *log.Logger
}

// NewFormBackfillHandler builds the handler; logger nil writes no line.
func NewFormBackfillHandler(backfills FormBackfills, logger *log.Logger) *FormBackfillHandler {
	return &FormBackfillHandler{backfills: backfills, logger: logger}
}

// Request is POST /v1/events/{id}/forms/{formId}/backfill: 202
// {"queued": true}, or 200 {"queued": false} when a backfill of the form
// waits already. 404 for an Event the caller cannot see (or archived), 403
// for one they may not edit, 409 event_cancelled, event_ended,
// form_not_listed or form_in_use, 503 forms_pull_off.
func (h *FormBackfillHandler) Request(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return eventError(c, err)
	}
	eventID, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	formID, err := uuid.Parse(c.Params("formId"))
	if err != nil || formID == uuid.Nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	queued, err := h.backfills.Request(c.Context(), p, eventID, formID)
	h.log(c, eventID, formID, queued, err)
	switch {
	case err == nil && queued:
		return c.Status(fiber.StatusAccepted).JSON(fiber.Map{"queued": true})
	case err == nil:
		return c.JSON(fiber.Map{"queued": false})
	case errors.Is(err, formsfeed.ErrBackfillEventNotFound):
		return problem(c, fiber.StatusNotFound, "Not Found")
	case errors.Is(err, formsfeed.ErrBackfillForbidden):
		return problem(c, fiber.StatusForbidden, "Forbidden")
	case errors.Is(err, formsfeed.ErrBackfillCancelled):
		return problemCode(c, fiber.StatusConflict, "Conflict", "event_cancelled")
	case errors.Is(err, formsfeed.ErrBackfillEnded):
		return problemCode(c, fiber.StatusConflict, "Conflict", "event_ended")
	case errors.Is(err, formsfeed.ErrBackfillNotListed):
		return problemCode(c, fiber.StatusConflict, "Conflict", "form_not_listed")
	case errors.Is(err, formsfeed.ErrBackfillFormInUse):
		return problemCode(c, fiber.StatusConflict, "Conflict", "form_in_use")
	case errors.Is(err, formsfeed.ErrBackfillOff):
		return problemCode(c, fiber.StatusServiceUnavailable, "Service Unavailable", "forms_pull_off")
	default:
		return err
	}
}

// log writes one line per request: the Event, the form and how it ended,
// never who asked.
func (h *FormBackfillHandler) log(c fiber.Ctx, eventID, formID uuid.UUID, queued bool, err error) {
	if h.logger == nil {
		return
	}
	outcome := "queued"
	switch {
	case err != nil:
		outcome = "refused"
	case !queued:
		outcome = "waiting"
	}
	payload, marshalErr := json.Marshal(struct {
		Event         string `json:"event"`
		CorrelationID string `json:"correlation_id"`
		EventID       string `json:"event_id"`
		FormID        string `json:"form_id"`
		Outcome       string `json:"outcome"`
	}{"form_backfill_request", requestid.FromContext(c), eventID.String(), formID.String(), outcome})
	if marshalErr == nil {
		h.logger.Print(string(payload))
	}
}
