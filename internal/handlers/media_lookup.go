package handlers

import (
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/limiter"
	"github.com/skylab-kulubu/core-backend/internal/authn"
	"github.com/skylab-kulubu/core-backend/internal/media"
)

// lookupBody is the body of POST /v1/media/lookup.
type lookupBody struct {
	OnBehalfOf string   `json:"onBehalfOf"`
	Addresses  []string `json:"addresses"`
}

// maxLookupBodyBytes is the largest lookup body read: 100 addresses of the
// longest the lookup reads (2 048 characters) with room for JSON's quoting.
const maxLookupBodyBytes = 256 << 10

// errLookupBodyTooLarge refuses a lookup body over maxLookupBodyBytes,
// unread.
var errLookupBodyTooLarge = errors.New("handlers: the lookup body is larger than a lookup takes")

// LookUp answers which Media each stored address names, for a product's
// service account acting for a person (the address lookup): 200 with one
// result per address, in order.
//
// The body is read only once the service has authorized the caller: a
// person always gets 403, whatever they sent. It is read as it came, not
// decoded from a Content-Encoding, and not at all past maxLookupBodyBytes.
func (h *MediaHandler) LookUp(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return mediaError(c, err)
	}
	results, err := h.svc.LookUp(c.Context(), p, func() (media.LookupRequest, error) {
		raw := c.Request().Body()
		if len(raw) > maxLookupBodyBytes {
			return media.LookupRequest{}, errLookupBodyTooLarge
		}
		var body lookupBody
		if err := json.Unmarshal(raw, &body); err != nil {
			return media.LookupRequest{}, media.ErrInvalid
		}
		return media.LookupRequest{OnBehalfOf: parsedID(body.OnBehalfOf), Addresses: body.Addresses}, nil
	})
	switch {
	case errors.Is(err, errLookupBodyTooLarge):
		return problemWithFields(c, fiber.StatusRequestEntityTooLarge, "Content Too Large",
			"The lookup body is larger than a lookup of maxAddresses addresses needs.", "media_lookup_too_large",
			fiber.Map{"maxBytes": maxLookupBodyBytes, "maxAddresses": media.MaxLookupAddresses})
	case errors.Is(err, media.ErrLookupTooMany):
		return problemWithFields(c, fiber.StatusBadRequest, "Bad Request",
			"A lookup takes at most maxAddresses addresses; send the rest in another.", "media_lookup_too_many",
			fiber.Map{"maxAddresses": media.MaxLookupAddresses})
	case err != nil:
		return attachError(c, err)
	}
	return c.JSON(fiber.Map{"results": results})
}

// DefaultMediaLookupsPerMinute is each product's lookup budget: 60 lookups
// a minute, 6 000 addresses.
const DefaultMediaLookupsPerMinute = 60

// LimitMediaLookups keeps each product's budget of address lookups:
// perMinute a minute (DefaultMediaLookupsPerMinute when not positive),
// counted for the product whose service account calls. A caller that is
// no product's service account passes untouched, so the route refuses it
// with 403 for who it is. The budget is this process's.
func LimitMediaLookups(perMinute int) fiber.Handler {
	if perMinute <= 0 {
		perMinute = DefaultMediaLookupsPerMinute
	}
	return limiter.New(limiter.Config{
		Max:          perMinute,
		Expiration:   time.Minute,
		Next:         func(c fiber.Ctx) bool { return lookupProduct(c) == "" },
		KeyGenerator: lookupProduct,
		LimitReached: func(c fiber.Ctx) error {
			// The limiter has set Retry-After already.
			seconds, _ := strconv.Atoi(string(c.Response().Header.Peek(fiber.HeaderRetryAfter)))
			return problemWithFields(c, fiber.StatusTooManyRequests, "Too Many Requests",
				"The product's lookup limit is reached; retry after the given seconds.", "media_lookup_rate_limited",
				fiber.Map{"maxLookups": perMinute, "windowSeconds": 60, "retryAfterSeconds": seconds})
		},
	})
}

// lookupProduct is the product whose service account calls, or "".
func lookupProduct(c fiber.Ctx) string {
	ident, ok := c.Locals(authn.LocalsIdentity).(authn.Identity)
	if !ok {
		return ""
	}
	return string(ident.Product)
}
