package handlers

import (
	"strconv"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/skylab-kulubu/core-backend/internal/authn"
	"github.com/skylab-kulubu/core-backend/internal/media"
)

// LimitMediaUploads charges each single-step upload to the signed-in
// person's budget before the route reads the form. Every route that stores a
// file sent through core shares the one limiter; a Direct upload grant is not
// charged here, its owning product limits it (ADR-0052).
//
// An anonymous request passes through untouched, so the route refuses it
// with 401 as before.
func LimitMediaUploads(limiter *media.UploadLimiter) fiber.Handler {
	return func(c fiber.Ctx) error {
		ident, ok := c.Locals(authn.LocalsIdentity).(authn.Identity)
		if !ok {
			return c.Next()
		}
		if refusal, ok := limiter.Admit(ident.ID, receivedBodyBytes(c)); !ok {
			return uploadRateLimited(c, refusal)
		}
		return c.Next()
	}
}

// receivedBodyBytes is the size of the request body core accepted. The server
// has already received the whole body (up to its body limit) before any
// handler runs, and a declared Content-Length is exactly what it read; the
// length is used rather than the body, which a pre-parsed multipart form would
// have to rebuild. A chunked body declares no length: its buffered bytes are
// counted instead, so leaving the header off does not dodge the budget.
func receivedBodyBytes(c fiber.Ctx) int64 {
	if declared := c.Request().Header.ContentLength(); declared >= 0 {
		return int64(declared)
	}
	return int64(len(c.Request().Body()))
}

func uploadRateLimited(c fiber.Ctx, refusal media.UploadLimitRefusal) error {
	seconds := int64(refusal.RetryAfter / time.Second)
	c.Set(fiber.HeaderRetryAfter, strconv.FormatInt(seconds, 10))
	fields := fiber.Map{
		"limit":             string(refusal.Limit),
		"windowSeconds":     int64(refusal.Window / time.Second),
		"retryAfterSeconds": seconds,
	}
	if refusal.Limit == media.UploadLimitVolume {
		fields["maxBytes"] = refusal.Max
	} else {
		fields["maxUploads"] = refusal.Max
	}
	return problemWithFields(c, fiber.StatusTooManyRequests, "Too Many Requests",
		"The person's upload limit is reached; retry after the given seconds.", "media_rate_limited", fields)
}
