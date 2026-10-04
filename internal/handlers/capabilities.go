package handlers

import (
	"errors"

	"github.com/gofiber/fiber/v3"
	"github.com/skylab-kulubu/core-backend/internal/authz"
)

// CapabilitiesHandler answers what the caller may do, from the authorizer
// that decides their requests (docs/authz-roles.md).
type CapabilitiesHandler struct {
	az authz.Authorizer
}

func NewCapabilitiesHandler(az authz.Authorizer) *CapabilitiesHandler {
	return &CapabilitiesHandler{az: az}
}

// Get answers GET /v1/users/me/capabilities. It is the caller's own answer,
// so no cache keeps it.
func (h *CapabilitiesHandler) Get(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		if errors.Is(err, fiber.ErrUnauthorized) {
			return problem(c, fiber.StatusUnauthorized, "Unauthorized")
		}
		return err
	}
	c.Set(fiber.HeaderCacheControl, "no-store")
	return c.JSON(h.az.Capabilities(p))
}
