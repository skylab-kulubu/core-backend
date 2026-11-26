package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/requestid"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/formsfeed"
)

// FormMappings serves an Event's form mappings (formsfeed.Mappings).
type FormMappings interface {
	Questions(ctx context.Context, p authz.Principal, eventID, formID uuid.UUID) (formsfeed.FormQuestions, error)
	Get(ctx context.Context, p authz.Principal, eventID, formID uuid.UUID) (formsfeed.Mapping, error)
	Put(ctx context.Context, p authz.Principal, m formsfeed.Mapping) (formsfeed.Mapping, bool, error)
	Delete(ctx context.Context, p authz.Principal, eventID, formID uuid.UUID) error
}

// FormMappingHandler is the panel's form mapping (docs/form-mappings.md).
type FormMappingHandler struct {
	mappings FormMappings
	logger   *log.Logger
}

// NewFormMappingHandler builds the handler; logger nil writes no line.
func NewFormMappingHandler(mappings FormMappings, logger *log.Logger) *FormMappingHandler {
	return &FormMappingHandler{mappings: mappings, logger: logger}
}

// mappingJSON is a mapping as the routes read and answer it: question ids,
// null for none. phone, university, department and grade are optional
// (a body without them names none).
type mappingJSON struct {
	FirstName  *string    `json:"firstName"`
	LastName   *string    `json:"lastName"`
	FullName   *string    `json:"fullName"`
	Email      *string    `json:"email"`
	Phone      *string    `json:"phone"`
	University *string    `json:"university"`
	Department *string    `json:"department"`
	Grade      *string    `json:"grade"`
	UpdatedAt  *time.Time `json:"updatedAt,omitempty"`
}

func idOrNull(id string) *string {
	if id == "" {
		return nil
	}
	return &id
}

func valueOf(id *string) string {
	if id == nil {
		return ""
	}
	return *id
}

func mappingOut(m formsfeed.Mapping, withTime bool) mappingJSON {
	out := mappingJSON{
		FirstName: idOrNull(m.FirstName), LastName: idOrNull(m.LastName), FullName: idOrNull(m.FullName), Email: idOrNull(m.Email),
		Phone: idOrNull(m.Phone), University: idOrNull(m.University), Department: idOrNull(m.Department), Grade: idOrNull(m.Grade),
	}
	if withTime && !m.UpdatedAt.IsZero() {
		at := m.UpdatedAt.UTC()
		out.UpdatedAt = &at
	}
	return out
}

// ids reads the route's Event and form.
func (h *FormMappingHandler) ids(c fiber.Ctx) (authz.Principal, uuid.UUID, uuid.UUID, error) {
	p, err := caller(c)
	if err != nil {
		return authz.Principal{}, uuid.Nil, uuid.Nil, eventError(c, err)
	}
	eventID, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return authz.Principal{}, uuid.Nil, uuid.Nil, problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	formID, err := uuid.Parse(c.Params("formId"))
	if err != nil || formID == uuid.Nil {
		return authz.Principal{}, uuid.Nil, uuid.Nil, problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	return p, eventID, formID, nil
}

// Questions is GET /v1/events/{id}/forms/{formId}/questions: 200 {formId,
// title, questions: [{id, type, question, inputType, identity}],
// suggestion: {firstName, lastName, fullName, email, …}} (the details are
// never suggested: null).
func (h *FormMappingHandler) Questions(c fiber.Ctx) error {
	p, eventID, formID, err := h.ids(c)
	if err != nil {
		return err
	}
	got, err := h.mappings.Questions(c.Context(), p, eventID, formID)
	h.log(c, "questions", eventID, formID, err)
	if err != nil {
		return h.error(c, err)
	}
	return c.JSON(fiber.Map{
		"formId": got.FormID, "title": got.Title, "questions": got.Questions, "suggestion": mappingOut(got.Suggestion, false),
	})
}

// Get is GET /v1/events/{id}/forms/{formId}/mapping: 200 the mapping, 404
// mapping_not_found without one.
func (h *FormMappingHandler) Get(c fiber.Ctx) error {
	p, eventID, formID, err := h.ids(c)
	if err != nil {
		return err
	}
	m, err := h.mappings.Get(c.Context(), p, eventID, formID)
	if err != nil {
		return h.error(c, err)
	}
	return c.JSON(mappingOut(m, true))
}

// Put is PUT /v1/events/{id}/forms/{formId}/mapping with {firstName,
// lastName, fullName, email, phone, university, department, grade}
// (question ids, null or "" for none): 201 when
// it is the first, 200 when it replaced one, with the mapping.
func (h *FormMappingHandler) Put(c fiber.Ctx) error {
	p, eventID, formID, err := h.ids(c)
	if err != nil {
		return err
	}
	var body mappingJSON
	if err := json.Unmarshal(c.Body(), &body); err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	saved, created, err := h.mappings.Put(c.Context(), p, formsfeed.Mapping{
		EventID: eventID, FormID: formID,
		FirstName: valueOf(body.FirstName), LastName: valueOf(body.LastName), FullName: valueOf(body.FullName), Email: valueOf(body.Email),
		Phone: valueOf(body.Phone), University: valueOf(body.University), Department: valueOf(body.Department), Grade: valueOf(body.Grade),
	})
	h.log(c, "put", eventID, formID, err)
	if err != nil {
		return h.error(c, err)
	}
	status := fiber.StatusOK
	if created {
		status = fiber.StatusCreated
	}
	return c.Status(status).JSON(mappingOut(saved, true))
}

// Delete is DELETE /v1/events/{id}/forms/{formId}/mapping: 204, 404
// mapping_not_found without one.
func (h *FormMappingHandler) Delete(c fiber.Ctx) error {
	p, eventID, formID, err := h.ids(c)
	if err != nil {
		return err
	}
	err = h.mappings.Delete(c.Context(), p, eventID, formID)
	h.log(c, "delete", eventID, formID, err)
	if err != nil {
		return h.error(c, err)
	}
	return c.SendStatus(fiber.StatusNoContent)
}

func (h *FormMappingHandler) error(c fiber.Ctx, err error) error {
	var missing *formsfeed.MappingQuestionMissingError
	switch {
	case errors.Is(err, formsfeed.ErrMappingEventNotFound):
		return problem(c, fiber.StatusNotFound, "Not Found")
	case errors.Is(err, formsfeed.ErrMappingForbidden):
		return problem(c, fiber.StatusForbidden, "Forbidden")
	case errors.Is(err, formsfeed.ErrFormsAccessDenied):
		return problemCode(c, fiber.StatusForbidden, "Forbidden", "forms_access_denied")
	case errors.Is(err, formsfeed.ErrMappingNotListed):
		return problemCode(c, fiber.StatusNotFound, "Not Found", "form_not_listed")
	case errors.Is(err, formsfeed.ErrMappingNotFound):
		return problemCode(c, fiber.StatusNotFound, "Not Found", "mapping_not_found")
	case errors.Is(err, formsfeed.ErrFormGone):
		return problemCode(c, fiber.StatusNotFound, "Not Found", "form_not_found")
	case errors.Is(err, formsfeed.ErrMappingInvalid):
		return problemCode(c, fiber.StatusBadRequest, "Bad Request", "mapping_invalid")
	case errors.Is(err, formsfeed.ErrMappingTooManyQuestions):
		return problemWithFields(c, fiber.StatusBadRequest, "Bad Request", "Bad Request",
			"mapping_too_many_questions", fiber.Map{"max": formsfeed.MaxFields})
	case errors.As(err, &missing):
		// The ids are the caller's own; never an answer.
		return problemWithFields(c, fiber.StatusUnprocessableEntity, "Unprocessable Entity", "Unprocessable Entity",
			"mapping_question_missing", fiber.Map{"missing": missing.Missing})
	case errors.Is(err, formsfeed.ErrFormsPullOff):
		return problemCode(c, fiber.StatusServiceUnavailable, "Service Unavailable", "forms_pull_off")
	case errors.Is(err, formsfeed.ErrFormsUnavailable):
		return problemCode(c, fiber.StatusServiceUnavailable, "Service Unavailable", "forms_unavailable")
	default:
		return err
	}
}

// log writes one line per questions read, save and delete: the Event, the
// form and how it ended, never who asked nor an answer.
func (h *FormMappingHandler) log(c fiber.Ctx, action string, eventID, formID uuid.UUID, err error) {
	if h.logger == nil {
		return
	}
	outcome, reason := "ok", ""
	switch {
	case errors.Is(err, formsfeed.ErrFormsUnavailable):
		outcome, reason = "forms_unavailable", "failed"
		switch {
		case errors.Is(err, formsfeed.ErrForbidden):
			reason = "forbidden"
		case errors.Is(err, formsfeed.ErrInvalid):
			reason = "invalid"
		}
	case err != nil:
		outcome = "refused"
	}
	payload, marshalErr := json.Marshal(struct {
		Event         string `json:"event"`
		CorrelationID string `json:"correlation_id"`
		Action        string `json:"action"`
		EventID       string `json:"event_id"`
		FormID        string `json:"form_id"`
		Outcome       string `json:"outcome"`
		Reason        string `json:"reason,omitempty"`
	}{"form_mapping", requestid.FromContext(c), action, eventID.String(), formID.String(), outcome, reason})
	if marshalErr == nil {
		h.logger.Print(string(payload))
	}
}
