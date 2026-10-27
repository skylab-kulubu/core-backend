package handlers

import (
	"errors"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/subjectrequest"
)

// SubjectRequestHandler serves the register of KVKK md. 11 data subject
// requests to the board (docs/subject-requests.md). Every answer is
// no-store: it names people.
type SubjectRequestHandler struct {
	requests subjectrequest.Service
}

func NewSubjectRequestHandler(requests subjectrequest.Service) *SubjectRequestHandler {
	return &SubjectRequestHandler{requests: requests}
}

func subjectRequestError(c fiber.Ctx, err error) error {
	var refused *subjectrequest.InvalidError
	switch {
	case errors.Is(err, fiber.ErrUnauthorized):
		return problem(c, fiber.StatusUnauthorized, "Unauthorized")
	case errors.Is(err, subjectrequest.ErrForbidden):
		return problem(c, fiber.StatusForbidden, "Forbidden")
	case errors.Is(err, subjectrequest.ErrNotFound):
		return problem(c, fiber.StatusNotFound, "Not Found")
	case errors.Is(err, subjectrequest.ErrConflict):
		return problemCode(c, fiber.StatusConflict, "Conflict", "subject_request_changed")
	case errors.As(err, &refused):
		return problemCode(c, fiber.StatusBadRequest, "Bad Request", refused.Code)
	default:
		return err
	}
}

func (h *SubjectRequestHandler) List(c fiber.Ctx) error {
	c.Set(fiber.HeaderCacheControl, "no-store")
	p, err := caller(c)
	if err != nil {
		return subjectRequestError(c, err)
	}
	items, err := h.requests.List(c.Context(), p)
	if err != nil {
		return subjectRequestError(c, err)
	}
	return c.JSON(items)
}

func (h *SubjectRequestHandler) Get(c fiber.Ctx) error {
	c.Set(fiber.HeaderCacheControl, "no-store")
	p, err := caller(c)
	if err != nil {
		return subjectRequestError(c, err)
	}
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return problem(c, fiber.StatusNotFound, "Not Found")
	}
	item, err := h.requests.Get(c.Context(), p, id)
	if err != nil {
		return subjectRequestError(c, err)
	}
	return c.JSON(item)
}

func (h *SubjectRequestHandler) Create(c fiber.Ctx) error {
	c.Set(fiber.HeaderCacheControl, "no-store")
	p, err := caller(c)
	if err != nil {
		return subjectRequestError(c, err)
	}
	var body subjectrequest.Input
	if err := c.Bind().Body(&body); err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	created, err := h.requests.Create(c.Context(), p, body)
	if err != nil {
		return subjectRequestError(c, err)
	}
	return c.Status(fiber.StatusCreated).JSON(created)
}

func (h *SubjectRequestHandler) Update(c fiber.Ctx) error {
	c.Set(fiber.HeaderCacheControl, "no-store")
	p, err := caller(c)
	if err != nil {
		return subjectRequestError(c, err)
	}
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return problem(c, fiber.StatusNotFound, "Not Found")
	}
	var body subjectrequest.Patch
	if err := c.Bind().Body(&body); err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	updated, err := h.requests.Update(c.Context(), p, id, body)
	if err != nil {
		return subjectRequestError(c, err)
	}
	return c.JSON(updated)
}
