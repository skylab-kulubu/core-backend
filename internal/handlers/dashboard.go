package handlers

import (
	"errors"

	"github.com/gofiber/fiber/v3"
	"github.com/skylab-kulubu/core-backend/internal/dashboard"
)

type DashboardHandler struct {
	svc dashboard.Service
}

func NewDashboardHandler(svc dashboard.Service) *DashboardHandler {
	return &DashboardHandler{svc: svc}
}

// Summary answers GET /v1/dashboard/summary: the admin panel's counts for
// the Events the caller may read the applications of, and the Members
// section for a caller who may read people (docs/dashboard-summary.md).
// It names people, so no cache keeps it.
func (h *DashboardHandler) Summary(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		if errors.Is(err, fiber.ErrUnauthorized) {
			return problem(c, fiber.StatusUnauthorized, "Unauthorized")
		}
		return err
	}
	summary, err := h.svc.Summary(c.Context(), p)
	if err != nil {
		return err
	}
	c.Set(fiber.HeaderCacheControl, "no-store")
	return c.JSON(summary)
}
