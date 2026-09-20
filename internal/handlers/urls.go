package handlers

import (
	"context"
	"errors"
	"strings"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authn"
	"github.com/skylab-kulubu/core-backend/internal/lifecycle"
	"github.com/skylab-kulubu/core-backend/internal/middlewares"
	"github.com/skylab-kulubu/core-backend/internal/qr"
	"github.com/skylab-kulubu/core-backend/internal/shorturl"
)

type URLHandler struct {
	svc              shorturl.Service
	parse            func(string) (authn.Identity, error)
	attributionGuard URLAttributionGuard
}

type URLAttributionGuard func(context.Context, uuid.UUID) bool

func NewURLHandler(svc shorturl.Service, parse func(string) (authn.Identity, error), guards ...URLAttributionGuard) *URLHandler {
	h := &URLHandler{svc: svc, parse: parse}
	if len(guards) > 0 {
		h.attributionGuard = guards[0]
	}
	return h
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
	u, err := h.svc.Redirect(c.Context(), c.Params("alias"), shorturl.Hit{
		IP:        hopIP(c),
		UserAgent: strings.Clone(c.Get(fiber.HeaderUserAgent)),
		Referer:   strings.Clone(c.Get(fiber.HeaderReferer)),
		UserID:    h.hopUserID(c),
	})
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
	visibility, err := lifecycleVisibility(c)
	if err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	var items []shorturl.URL
	if visibility == lifecycle.CurrentOnly {
		items, err = h.svc.ListMine(c.Context(), p)
	} else {
		items, err = h.svc.ListMineLifecycle(c.Context(), p, visibility)
	}
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
	visibility, err := lifecycleVisibility(c)
	if err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	var items []shorturl.URL
	if visibility == lifecycle.CurrentOnly {
		items, err = h.svc.ListAll(c.Context(), p)
	} else {
		items, err = h.svc.ListAllLifecycle(c.Context(), p, visibility)
	}
	if err != nil {
		return urlError(c, err)
	}
	return c.JSON(items)
}

func (h *URLHandler) Restore(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return urlError(c, err)
	}
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	restored, err := h.svc.Restore(c.Context(), p, id)
	if err != nil {
		return urlError(c, err)
	}
	return c.JSON(restored)
}

func (h *URLHandler) ListHits(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return urlError(c, err)
	}
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	hits, err := h.svc.ListHits(c.Context(), p, id)
	if err != nil {
		return urlError(c, err)
	}
	return c.JSON(hits)
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

func (h *URLHandler) hopUserID(c fiber.Ctx) *uuid.UUID {
	if h.attributionGuard == nil {
		return nil
	}
	if ident, ok := c.Locals(authn.LocalsIdentity).(authn.Identity); ok && ident.ID != uuid.Nil {
		if h.attributionGuard(c.Context(), ident.ID) {
			id := ident.ID
			return &id
		}
		return nil
	}
	ident, err := middlewares.IdentityFromBearer(c.Get(fiber.HeaderAuthorization), h.parse)
	if err != nil || ident.ID == uuid.Nil {
		return nil
	}
	if !h.attributionGuard(c.Context(), ident.ID) {
		return nil
	}
	id := ident.ID
	return &id
}

func hopIP(c fiber.Ctx) string {
	if xff := c.Get(fiber.HeaderXForwardedFor); xff != "" {
		return strings.Clone(strings.TrimSpace(strings.Split(xff, ",")[0]))
	}
	return strings.Clone(c.IP())
}
