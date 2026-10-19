package middlewares

import (
	"strings"

	"github.com/gofiber/fiber/v3"
)

// InternalRoutePrefix starts the routes meant only for the other services
// on the Docker network (ADR-0016), never the public edge.
const InternalRoutePrefix = "/internal"

// InternalRouteGuard answers a bare 404 to a request under
// InternalRoutePrefix that came through the public ingress, before any
// token is read: Traefik adds forwarding headers to everything it passes
// on, and a service calling core over the internal network sends none. It
// is a second layer only: any container on the network reaches core
// without Traefik, so the token checks behind it stay the boundary (the
// same guard as SkyMail's, docs/account-erasure-command.md).
func InternalRouteGuard() fiber.Handler {
	return func(c fiber.Ctx) error {
		if cameThroughIngress(c) {
			c.Set(fiber.HeaderCacheControl, "no-store")
			return c.SendStatus(fiber.StatusNotFound)
		}
		return c.Next()
	}
}

func cameThroughIngress(c fiber.Ctx) bool {
	for name := range c.GetReqHeaders() {
		name = strings.ToLower(name)
		if strings.HasPrefix(name, "x-forwarded-") || name == "forwarded" || name == "x-real-ip" {
			return true
		}
	}
	return false
}
