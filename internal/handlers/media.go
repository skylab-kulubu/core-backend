package handlers

import (
	"errors"
	"io"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/media"
)

type MediaHandler struct {
	svc media.Service
}

func NewMediaHandler(svc media.Service) *MediaHandler {
	return &MediaHandler{svc: svc}
}

func mediaError(c fiber.Ctx, err error) error {
	switch {
	case errors.Is(err, fiber.ErrUnauthorized):
		return problem(c, fiber.StatusUnauthorized, "Unauthorized")
	case errors.Is(err, media.ErrForbidden):
		return problem(c, fiber.StatusForbidden, "Forbidden")
	case errors.Is(err, media.ErrNotFound):
		return problem(c, fiber.StatusNotFound, "Not Found")
	case errors.Is(err, media.ErrInvalid):
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	default:
		return err
	}
}

func (h *MediaHandler) Upload(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return mediaError(c, err)
	}
	header, err := c.FormFile("file")
	if err != nil || header == nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	f, err := header.Open()
	if err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	defer f.Close()
	data, err := io.ReadAll(f)
	if err != nil {
		return err
	}
	created, err := h.svc.Upload(c.Context(), p, header.Filename, header.Header.Get("Content-Type"), data)
	if err != nil {
		return mediaError(c, err)
	}
	return c.Status(fiber.StatusCreated).JSON(created)
}

func (h *MediaHandler) Get(c fiber.Ctx) error {
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	got, err := h.svc.Get(c.Context(), id)
	if err != nil {
		return mediaError(c, err)
	}
	return c.JSON(got)
}

func (h *MediaHandler) List(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return mediaError(c, err)
	}
	items, err := h.svc.List(c.Context(), p)
	if err != nil {
		return mediaError(c, err)
	}
	return c.JSON(items)
}

func (h *MediaHandler) Delete(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return mediaError(c, err)
	}
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	if err := h.svc.Delete(c.Context(), p, id); err != nil {
		return mediaError(c, err)
	}
	return c.SendStatus(fiber.StatusNoContent)
}
