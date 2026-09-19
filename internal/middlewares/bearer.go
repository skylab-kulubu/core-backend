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
