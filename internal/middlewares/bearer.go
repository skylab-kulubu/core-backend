package middlewares

import (
	"strings"

	"github.com/gofiber/fiber/v3"
	"github.com/skylab-kulubu/core-backend/internal/authn"
)

func Bearer(parse func(string) (authn.Identity, error)) fiber.Handler {
	if parse == nil {
		parse = authn.ParseAccessToken
	}
	return func(c fiber.Ctx) error {
		header := c.Get(fiber.HeaderAuthorization)
		if header == "" {
			return c.Next()
		}
		if !strings.HasPrefix(header, "Bearer ") {
			return fiber.ErrUnauthorized
		}
		ident, err := parse(strings.TrimPrefix(header, "Bearer "))
		if err != nil {
			return fiber.ErrUnauthorized
		}
		c.Locals(authn.LocalsIdentity, ident)
		return c.Next()
	}
}
