package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/formsfeed"
	"github.com/skylab-kulubu/core-backend/internal/teamaudit"
)

// FormRespondents serves an Event's Unticketed respondents
// (formsfeed.Respondents).
type FormRespondents interface {
	List(ctx context.Context, p authz.Principal, eventID uuid.UUID, q formsfeed.RespondentQuery) (formsfeed.RespondentPage, bool, error)
	Preview(ctx context.Context, p authz.Principal, eventID uuid.UUID, sel formsfeed.Selection) (formsfeed.Preview, error)
	Send(ctx context.Context, p authz.Principal, eventID uuid.UUID, sel formsfeed.Selection) (formsfeed.SendResult, *uuid.UUID, error)
	Job(ctx context.Context, p authz.Principal, eventID, jobID uuid.UUID) (formsfeed.TicketJob, error)
}

// FormRespondentHandler is the panel's "Bilet gönderilmedi" and "Forms'ta
// onay bekliyor" list and its sending (docs/form-respondents.md).
type FormRespondentHandler struct {
	respondents FormRespondents
}

// NewFormRespondentHandler builds the handler.
func NewFormRespondentHandler(respondents FormRespondents) *FormRespondentHandler {
	return &FormRespondentHandler{respondents: respondents}
}

// respondentJSON is one Unticketed respondent as the list answers it.
type respondentJSON struct {
	ID          uuid.UUID  `json:"id"`
	FormID      uuid.UUID  `json:"formId"`
	ResponseID  uuid.UUID  `json:"responseId"`
	UserID      *uuid.UUID `json:"userId"`
	FirstName   string     `json:"firstName"`
	LastName    string     `json:"lastName"`
	Email       string     `json:"email"`
	SubmittedAt *time.Time `json:"submittedAt"`
	FormsStatus string     `json:"formsStatus"`
	Source      string     `json:"source"`
}

func (h *FormRespondentHandler) event(c fiber.Ctx) (authz.Principal, uuid.UUID, error) {
	p, err := caller(c)
	if err != nil {
		return authz.Principal{}, uuid.Nil, eventError(c, err)
	}
	eventID, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return authz.Principal{}, uuid.Nil, problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	return p, eventID, nil
}

func invalidRespondents(c fiber.Ctx) error {
	return problemCode(c, fiber.StatusBadRequest, "Bad Request", "respondents_invalid")
}

// List is GET /v1/events/{id}/respondents?formId=&formsStatus=accepted|pending&q=&after=&limit=:
// 200 {items, nextCursor, total, collecting}.
func (h *FormRespondentHandler) List(c fiber.Ctx) error {
	p, eventID, err := h.event(c)
	if err != nil {
		return err
	}
	q := formsfeed.RespondentQuery{FormsStatus: c.Query("formsStatus"), Q: c.Query("q"), After: c.Query("after")}
	if raw := c.Query("formId"); raw != "" {
		id, err := uuid.Parse(raw)
		if err != nil {
			return invalidRespondents(c)
		}
		q.FormID = &id
	}
	if raw := c.Query("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > formsfeed.MaxRespondentPage {
			return invalidRespondents(c)
		}
		q.Limit = n
	}
	page, collecting, err := h.respondents.List(c.Context(), p, eventID, q)
	if err != nil {
		return h.error(c, err)
	}
	items := make([]respondentJSON, 0, len(page.Items))
	for _, r := range page.Items {
		items = append(items, respondentJSON{
			ID: r.ID, FormID: r.FormID, ResponseID: r.ResponseID, UserID: r.UserID,
			FirstName: r.FirstName, LastName: r.LastName, Email: r.Email,
			SubmittedAt: r.SubmittedAt, FormsStatus: r.FormsStatus, Source: r.Source,
		})
	}
	var next *string
	if page.NextCursor != "" {
		next = &page.NextCursor
	}
	c.Set(fiber.HeaderCacheControl, "no-store")
	return c.JSON(fiber.Map{"items": items, "nextCursor": next, "total": page.Total, "collecting": collecting, "incomplete": page.Incomplete})
}

// selectionJSON is the body of the preview and the send: {ids: [...]} or
// {all: true, formId?}.
type selectionJSON struct {
	IDs    []uuid.UUID `json:"ids"`
	All    bool        `json:"all"`
	FormID *uuid.UUID  `json:"formId"`
}

func (h *FormRespondentHandler) selection(c fiber.Ctx) (formsfeed.Selection, bool) {
	var body selectionJSON
	if err := json.Unmarshal(c.Body(), &body); err != nil {
		return formsfeed.Selection{}, false
	}
	sel := formsfeed.Selection{IDs: body.IDs, All: body.All, FormID: body.FormID}
	if !sel.All {
		sel.FormID = nil
	}
	return sel, true
}

// Preview is POST /v1/events/{id}/respondents/tickets/preview: 200
// {tickets, waitlisted, full, alreadyTicketed, pendingSkipped}.
func (h *FormRespondentHandler) Preview(c fiber.Ctx) error {
	p, eventID, err := h.event(c)
	if err != nil {
		return err
	}
	sel, ok := h.selection(c)
	if !ok {
		return invalidRespondents(c)
	}
	got, err := h.respondents.Preview(c.Context(), p, eventID, sel)
	if err != nil {
		return h.error(c, err)
	}
	return c.JSON(got)
}

// Send is POST /v1/events/{id}/respondents/tickets: ids → 200 {recorded,
// waitlisted, full, skipped}; all → 202 {jobId}. The sending's Team audit
// row names the caller (auditContext): without them it is refused 503.
func (h *FormRespondentHandler) Send(c fiber.Ctx) error {
	p, eventID, err := h.event(c)
	if err != nil {
		return err
	}
	sel, ok := h.selection(c)
	if !ok {
		return invalidRespondents(c)
	}
	res, jobID, err := h.respondents.Send(auditContext(c, p), p, eventID, sel)
	if err != nil {
		return h.error(c, err)
	}
	if jobID != nil {
		return c.Status(fiber.StatusAccepted).JSON(fiber.Map{"jobId": *jobID})
	}
	return c.JSON(res)
}

// Job is GET /v1/events/{id}/respondents/ticket-jobs/{jobId}: 200 {jobId,
// formId, state, recorded, waitlisted, full, skipped, createdAt, updatedAt}.
func (h *FormRespondentHandler) Job(c fiber.Ctx) error {
	p, eventID, err := h.event(c)
	if err != nil {
		return err
	}
	jobID, err := uuid.Parse(c.Params("jobId"))
	if err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	job, err := h.respondents.Job(c.Context(), p, eventID, jobID)
	if err != nil {
		return h.error(c, err)
	}
	c.Set(fiber.HeaderCacheControl, "no-store")
	return c.JSON(job)
}

func (h *FormRespondentHandler) error(c fiber.Ctx, err error) error {
	switch {
	case errors.Is(err, formsfeed.ErrMappingEventNotFound):
		return problem(c, fiber.StatusNotFound, "Not Found")
	case errors.Is(err, formsfeed.ErrMappingForbidden):
		return problem(c, fiber.StatusForbidden, "Forbidden")
	case errors.Is(err, formsfeed.ErrTicketJobNotFound):
		return problemCode(c, fiber.StatusNotFound, "Not Found", "ticket_job_not_found")
	case errors.Is(err, formsfeed.ErrRespondentsInvalid):
		return invalidRespondents(c)
	case errors.Is(err, formsfeed.ErrRespondentsEventCancelled):
		return problemCode(c, fiber.StatusConflict, "Conflict", "event_cancelled")
	case errors.Is(err, formsfeed.ErrRespondentsEventEnded):
		return problemCode(c, fiber.StatusConflict, "Conflict", "event_ended")
	case errors.Is(err, teamaudit.ErrUnavailable):
		return problemCode(c, fiber.StatusServiceUnavailable, "Service Unavailable", auditUnavailableCode)
	default:
		return err
	}
}
