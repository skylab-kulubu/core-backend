package middlewares

import (
	"strings"

	"github.com/gofiber/fiber/v3"
	"github.com/skylab-kulubu/core-backend/internal/authn"
)

func Bearer(c fiber.Ctx) error {
	header := c.Get(fiber.HeaderAuthorization)
	if header == "" {
		return c.Next()
	}
	if !strings.HasPrefix(header, "Bearer ") {
		return fiber.ErrUnauthorized
	}
	ident, err := authn.ParseAccessToken(strings.TrimPrefix(header, "Bearer "))
	if err != nil {
		return fiber.ErrUnauthorized
	}
	c.Locals(authn.LocalsIdentity, ident)
	return c.Next()
}
