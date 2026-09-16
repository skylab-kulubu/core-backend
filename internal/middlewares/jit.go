package middlewares

import (
	"github.com/gofiber/fiber/v3"
	"github.com/skylab-kulubu/core-backend/internal/authn"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

type JIT struct {
	users user.Service
}

func NewJIT(users user.Service) *JIT {
	return &JIT{users: users}
}

func (j *JIT) Handle(c fiber.Ctx) error {
	ident, ok := c.Locals(authn.LocalsIdentity).(authn.Identity)
	if !ok {
		return c.Next()
	}
	u, err := j.users.Ensure(c.Context(), ident.ID, ident.Profile)
	if err != nil {
		return err
	}
	c.Locals(authn.LocalsUser, u)
	return c.Next()
}
