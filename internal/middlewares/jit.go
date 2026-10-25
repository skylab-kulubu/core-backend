package middlewares

import (
	"errors"

	"github.com/gofiber/fiber/v3"
	"github.com/skylab-kulubu/core-backend/internal/authn"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// Waker is told when JIT opened a person's row: the user sync
// (docs/user-sync.md) then runs soon instead of at its next tick, and sends
// the Welcome mail if the person is owed one. JIT itself sends no mail.
type Waker interface {
	Wake()
}

type JIT struct {
	users user.Service
	wake  Waker
}

func NewJIT(users user.Service, wakers ...Waker) *JIT {
	j := &JIT{users: users}
	if len(wakers) > 0 {
		j.wake = wakers[0]
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
		if errors.Is(err, user.ErrAccountBlocked) {
			return fiber.ErrUnauthorized
		}
		if errors.Is(err, user.ErrConflict) {
			return c.Next()
		}
		return err
	}
	if created && j.wake != nil {
		j.wake.Wake()
	}
	// The user carries its profile picture as stored; the handlers that
	// answer with it build its address from the configured base.
	c.Locals(authn.LocalsUser, u)
	return c.Next()
}
