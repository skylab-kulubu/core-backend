package handlers

import (
	"errors"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/qr"
	"github.com/skylab-kulubu/core-backend/internal/shorturl"
)

type URLHandler struct {
	svc shorturl.Service
}

func NewURLHandler(svc shorturl.Service) *URLHandler {
	return &URLHandler{svc: svc}
}

type urlBody struct {
	URL   string `json:"url"`
	Alias string `json:"alias"`
}

func urlError(c fiber.Ctx, err error) error {
	switch {
	case errors.Is(err, fiber.ErrUnauthorized):
		return problem(c, fiber.StatusUnauthorized, "Unauthorized")
	case errors.Is(err, shorturl.ErrForbidden):
		return problem(c, fiber.StatusForbidden, "Forbidden")
	case errors.Is(err, shorturl.ErrNotFound):
		return problem(c, fiber.StatusNotFound, "Not Found")
	case errors.Is(err, shorturl.ErrConflict):
		return problem(c, fiber.StatusConflict, "Conflict")
	case errors.Is(err, shorturl.ErrInvalid):
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	default:
		return err
	}
}

func (h *URLHandler) Redirect(c fiber.Ctx) error {
	u, err := h.svc.Redirect(c.Context(), c.Params("alias"))
	if err != nil {
		return urlError(c, err)
	}
	c.Set(fiber.HeaderLocation, u.URL)
	return c.SendStatus(fiber.StatusMovedPermanently)
}

func (h *URLHandler) QR(c fiber.Ctx) error {
	u, err := h.svc.Lookup(c.Context(), c.Params("alias"))
	if err != nil {
		return urlError(c, err)
	}
	size := qr.SizeFromQuery(c.Query("size"))
	content := qr.ShortURL(u.Alias)
	var png []byte
	if qr.LogoFromQuery(c.Query("logo")) {
		png, err = qr.PNGWithLogo(content, size)
	} else {
		png, err = qr.PNG(content, size)
	}
	if err != nil {
		return err
	}
	c.Set(fiber.HeaderContentType, "image/png")
	return c.Send(png)
}

func (h *URLHandler) Create(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return urlError(c, err)
	}
	var b urlBody
	if err := c.Bind().Body(&b); err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	created, err := h.svc.Create(c.Context(), p, b.URL, b.Alias)
	if err != nil {
		return urlError(c, err)
	}
	return c.Status(fiber.StatusCreated).JSON(created)
}

func (h *URLHandler) ListMine(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return urlError(c, err)
	}
	items, err := h.svc.ListMine(c.Context(), p)
	if err != nil {
		return urlError(c, err)
	}
	return c.JSON(items)
}

func (h *URLHandler) ListAll(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return urlError(c, err)
	}
	items, err := h.svc.ListAll(c.Context(), p)
	if err != nil {
		return urlError(c, err)
	}
	return c.JSON(items)
}

func (h *URLHandler) Update(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return urlError(c, err)
	}
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	var b urlBody
	if err := c.Bind().Body(&b); err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	updated, err := h.svc.Update(c.Context(), p, id, b.URL, b.Alias)
	if err != nil {
		return urlError(c, err)
	}
	return c.JSON(updated)
}

func (h *URLHandler) Delete(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return urlError(c, err)
	}
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	if err := h.svc.Delete(c.Context(), p, id); err != nil {
		return urlError(c, err)
	}
	return c.SendStatus(fiber.StatusNoContent)
}
