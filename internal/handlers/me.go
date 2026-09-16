package handlers

import (
	"github.com/gofiber/fiber/v3"
	"github.com/skylab-kulubu/core-backend/internal/authn"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

type MeHandler struct{}

func NewMeHandler() *MeHandler {
	return &MeHandler{}
}

func (h *MeHandler) GetMe(c fiber.Ctx) error {
	u, ok := c.Locals(authn.LocalsUser).(user.User)
	if !ok {
		return fiber.ErrUnauthorized
	}
	return c.JSON(u)
}
