package middlewares

import (
	"errors"
	"github.com/gofiber/fiber/v3"
	"github.com/skylab-kulubu/core-backend/internal/authn"
	"github.com/skylab-kulubu/core-backend/internal/mail"
	"github.com/skylab-kulubu/core-backend/internal/media"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

type JIT struct {
	users user.Service
	mail  mail.Mailer
}

func NewJIT(users user.Service, mailers ...mail.Mailer) *JIT {
	j := &JIT{users: users}
	if len(mailers) > 0 {
		j.mail = mailers[0]
	}
	return j
}

func (j *JIT) Handle(c fiber.Ctx) error {
	ident, ok := c.Locals(authn.LocalsIdentity).(authn.Identity)
	if !ok {
		return c.Next()
	}
	u, created, err := j.users.Ensure(c.Context(), ident.ID, ident.Profile)
	if err != nil {
		if errors.Is(err, user.ErrConflict) {
			return c.Next()
		}
		return err
	}
	if created && j.mail != nil {
		j.mail.Welcome(c.Context(), u)
	}
	u.ProfilePictureURL = media.PublicURL("", u.ProfilePictureURL)
	c.Locals(authn.LocalsUser, u)
	return c.Next()
}
