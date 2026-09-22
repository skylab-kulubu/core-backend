package handlers

import (
	"context"
	"errors"
	"strings"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authn"
	"github.com/skylab-kulubu/core-backend/internal/clientip"
	"github.com/skylab-kulubu/core-backend/internal/lifecycle"
	"github.com/skylab-kulubu/core-backend/internal/middlewares"
	"github.com/skylab-kulubu/core-backend/internal/qr"
	"github.com/skylab-kulubu/core-backend/internal/shorturl"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

var errURLAttributionUnavailable = errors.New("url attribution unavailable")

type URLHandler struct {
	svc              shorturl.Service
	parse            func(string) (authn.Identity, error)
	attributionGuard URLAttributionGuard
	trustedProxies   clientip.Ranges
}

type URLAttributionGuard func(context.Context, uuid.UUID) (user.AttributionState, error)

func NewURLHandler(svc shorturl.Service, parse func(string) (authn.Identity, error), guards ...URLAttributionGuard) *URLHandler {
	h := &URLHandler{svc: svc, parse: parse}
	if len(guards) > 0 {
		h.attributionGuard = guards[0]
	}
	return h
}

// TrustProxies names the peers whose forwarded-for header may be read when a
// hop is recorded. Without it the handler records the socket address, which is
// the edge proxy itself rather than the person who clicked.
func (h *URLHandler) TrustProxies(ranges clientip.Ranges) *URLHandler {
	h.trustedProxies = ranges
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
	case errors.Is(err, user.ErrAccountBlocked):
		c.Set(fiber.HeaderCacheControl, "no-store")
		c.Set(fiber.HeaderWWWAuthenticate, `Bearer error="invalid_token"`)
		return problem(c, fiber.StatusUnauthorized, "Unauthorized")
	case errors.Is(err, errURLAttributionUnavailable):
		c.Set(fiber.HeaderCacheControl, "no-store")
		c.Set(fiber.HeaderRetryAfter, "1")
		return problem(c, fiber.StatusServiceUnavailable, "Service Unavailable")
	default:
		return err
	}
}

func (h *URLHandler) Redirect(c fiber.Ctx) error {
	userID, err := h.hopUserID(c)
	if err != nil {
		return urlError(c, err)
	}
	u, err := h.svc.Redirect(c.Context(), c.Params("alias"), shorturl.Hit{
		IP:        h.hopIP(c),
		UserAgent: strings.Clone(c.Get(fiber.HeaderUserAgent)),
		Referer:   strings.Clone(c.Get(fiber.HeaderReferer)),
		UserID:    userID,
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

func (h *URLHandler) hopUserID(c fiber.Ctx) (*uuid.UUID, error) {
	if h.attributionGuard == nil {
		return nil, nil
	}
	if ident, ok := c.Locals(authn.LocalsIdentity).(authn.Identity); ok && ident.ID != uuid.Nil {
		state, err := h.attributionGuard(c.Context(), ident.ID)
		if err != nil {
			return nil, errURLAttributionUnavailable
		}
		switch state {
		case user.AttributionAllowed:
			id := ident.ID
			return &id, nil
		case user.AttributionAnonymous:
			return nil, nil
		case user.AttributionBlocked:
			return nil, user.ErrAccountBlocked
		default:
			return nil, errURLAttributionUnavailable
		}
	}
	ident, err := middlewares.IdentityFromBearer(c.Get(fiber.HeaderAuthorization), h.parse)
	if err != nil || ident.ID == uuid.Nil {
		return nil, nil
	}
	state, err := h.attributionGuard(c.Context(), ident.ID)
	if err != nil {
		return nil, errURLAttributionUnavailable
	}
	switch state {
	case user.AttributionAllowed:
		id := ident.ID
		return &id, nil
	case user.AttributionAnonymous:
		return nil, nil
	case user.AttributionBlocked:
		return nil, user.ErrAccountBlocked
	default:
		return nil, errURLAttributionUnavailable
	}
}

// hopIP is the address recorded on a short-link hit. It is the address the
// edge proxy accepted the click from, never the leftmost forwarded-for entry:
// that end of the chain belongs to the caller, who would otherwise choose what
// url_hits.ip says about them. A value that does not parse as an address is
// stored as "" rather than as the text that was sent.
func (h *URLHandler) hopIP(c fiber.Ctx) string {
	return clientip.FromCtx(c, h.trustedProxies)
}
