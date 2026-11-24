package middlewares

import (
	"strings"

	"github.com/gofiber/fiber/v3"
	"github.com/skylab-kulubu/core-backend/internal/authn"
)

func Bearer(parse func(string) (authn.Identity, error)) fiber.Handler {
	return func(c fiber.Ctx) error {
		header := c.Get(fiber.HeaderAuthorization)
		if header == "" {
			return c.Next()
		}
		ident, err := IdentityFromBearer(header, parse)
		if err != nil {
			return fiber.ErrUnauthorized
		}
		c.Locals(authn.LocalsIdentity, ident)
		return c.Next()
	}
}

// LocalsBearerRejected marks a request whose bearer token OptionalBearer
// could not verify.
const LocalsBearerRejected = "bearer_rejected"

// OptionalBearer reads a bearer token like Bearer, except that a token it
// cannot verify leaves the request anonymous instead of answering 401, and is
// marked LocalsBearerRejected for the route to decide. Guest apply uses it:
// the internal network may call it without a token, while from the edge a
// rejected token is 401 and no token 404 (docs/guest-apply.md).
func OptionalBearer(parse func(string) (authn.Identity, error)) fiber.Handler {
	return func(c fiber.Ctx) error {
		header := c.Get(fiber.HeaderAuthorization)
		if header == "" {
			return c.Next()
		}
		if ident, err := IdentityFromBearer(header, parse); err == nil {
			c.Locals(authn.LocalsIdentity, ident)
		} else {
			c.Locals(LocalsBearerRejected, true)
		}
		return c.Next()
	}
}

func IdentityFromBearer(header string, parse func(string) (authn.Identity, error)) (authn.Identity, error) {
	if parse == nil || !strings.HasPrefix(header, "Bearer ") {
		return authn.Identity{}, fiber.ErrUnauthorized
	}
	ident, err := parse(strings.TrimPrefix(header, "Bearer "))
	if err != nil {
		return authn.Identity{}, err
	}
	return ident, nil
}
