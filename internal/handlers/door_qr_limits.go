package handlers

import (
	"strconv"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/limiter"
	"github.com/skylab-kulubu/core-backend/internal/authn"
	"github.com/skylab-kulubu/core-backend/internal/clientip"
)

// DoorQRLimits are the budgets of the door QR routes
// (docs/guest-self-check-in.md).
type DoorQRLimits struct {
	// GuestFailuresPerClient is how many failed guest check-ins (anything
	// answered 400 or above, already-checked-in included) one client address
	// may make per GuestWindow. Successful check-ins do not count, so a crowd
	// behind one campus NAT is not refused for checking in.
	GuestFailuresPerClient int
	GuestWindow            time.Duration
	// MintsPerSubject is how many door QRs one person may mint per
	// MintWindow: a screen asks for one every refreshAfterSeconds (15 s).
	MintsPerSubject int
	MintWindow      time.Duration
}

func DefaultDoorQRLimits() DoorQRLimits {
	return DoorQRLimits{
		GuestFailuresPerClient: 10, GuestWindow: time.Minute,
		MintsPerSubject: 30, MintWindow: time.Minute,
	}
}

// GuestCheckInLimit budgets Guest check-in by client address.
func (l DoorQRLimits) GuestCheckInLimit(proxies clientip.Ranges) fiber.Handler {
	return limiter.New(limiter.Config{
		Max:                    l.GuestFailuresPerClient,
		Expiration:             l.GuestWindow,
		SkipSuccessfulRequests: true,
		KeyGenerator: func(c fiber.Ctx) string {
			return clientip.FromCtx(c, proxies)
		},
		LimitReached: doorQRRateLimited("guest_check_in_rate_limited"),
	})
}

// MintLimit budgets door QR minting by the caller's token subject. Requests
// without an identity pass through to the handler, which answers 401.
func (l DoorQRLimits) MintLimit() fiber.Handler {
	return limiter.New(limiter.Config{
		Max:        l.MintsPerSubject,
		Expiration: l.MintWindow,
		Next: func(c fiber.Ctx) bool {
			_, ok := c.Locals(authn.LocalsIdentity).(authn.Identity)
			return !ok
		},
		KeyGenerator: func(c fiber.Ctx) string {
			ident, _ := c.Locals(authn.LocalsIdentity).(authn.Identity)
			return ident.ID.String()
		},
		LimitReached: doorQRRateLimited("door_qr_rate_limited"),
	})
}

func doorQRRateLimited(code string) fiber.Handler {
	return func(c fiber.Ctx) error {
		seconds, _ := strconv.Atoi(string(c.Response().Header.Peek(fiber.HeaderRetryAfter)))
		return problemWithFields(c, fiber.StatusTooManyRequests, "Too Many Requests",
			"Too many requests; retry after the given seconds.", code, fiber.Map{"retryAfterSeconds": seconds})
	}
}
