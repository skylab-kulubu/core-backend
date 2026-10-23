package handlers

import (
	"errors"

	"github.com/gofiber/fiber/v3"
	"github.com/skylab-kulubu/core-backend/internal/identity"
)

// AppsHandler answers the app assignment routes (docs/app-assignments.md):
// the panel's Uygulamalar page, Entra's Enterprise apps → "Users and
// groups". They are mounted only with APP_ASSIGNMENTS=on.
type AppsHandler struct {
	svc identity.Service
}

func NewAppsHandler(svc identity.Service) *AppsHandler {
	return &AppsHandler{svc: svc}
}

// notAssignedCode answers a removal of an app role the target does not
// hold directly.
const notAssignedCode = "not_assigned"

func appsError(c fiber.Ctx, err error) error {
	if errors.Is(err, identity.ErrAppNotAssigned) {
		return problemCode(c, fiber.StatusNotFound, "Not Found", notAssignedCode)
	}
	return identityError(c, err)
}

// List answers GET /v1/apps.
func (h *AppsHandler) List(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return identityError(c, err)
	}
	apps, err := h.svc.Apps(c.Context(), p)
	if err != nil {
		return appsError(c, err)
	}
	c.Set(fiber.HeaderCacheControl, "no-store")
	return c.JSON(apps)
}

// Assignments answers GET /v1/apps/:clientId/assignments?type=&role=.
func (h *AppsHandler) Assignments(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return identityError(c, err)
	}
	out, err := h.svc.AppAssignments(c.Context(), p, c.Params("clientId"), identity.AppAssignmentFilter{
		Type: c.Query("type"), Role: c.Query("role"),
	})
	if err != nil {
		return appsError(c, err)
	}
	c.Set(fiber.HeaderCacheControl, "no-store")
	return c.JSON(out)
}

// Assign answers POST /v1/apps/:clientId/assignments {type, id, role,
// note?}: 201 with the row, 200 with it when the target held the role
// already (nothing changed, no audit row).
func (h *AppsHandler) Assign(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return identityError(c, err)
	}
	var in identity.AppAssignmentInput
	if err := c.Bind().Body(&in); err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	row, created, err := h.svc.AssignApp(auditContext(c, p), p, c.Params("clientId"), in)
	if err != nil {
		return appsError(c, err)
	}
	if created {
		c.Status(fiber.StatusCreated)
	}
	return c.JSON(row)
}

// Unassign answers DELETE /v1/apps/:clientId/assignments with the same body
// as Assign: 204, or 404 not_assigned when the target does not hold the role
// directly.
func (h *AppsHandler) Unassign(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return identityError(c, err)
	}
	var in identity.AppAssignmentInput
	if err := c.Bind().Body(&in); err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	if err := h.svc.UnassignApp(auditContext(c, p), p, c.Params("clientId"), in); err != nil {
		return appsError(c, err)
	}
	return c.SendStatus(fiber.StatusNoContent)
}

// Audit answers GET /v1/apps/:clientId/audit: the app's last hundred rows of
// the Team audit record.
func (h *AppsHandler) Audit(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return identityError(c, err)
	}
	rows, err := h.svc.AppAudit(c.Context(), p, c.Params("clientId"))
	if err != nil {
		return appsError(c, err)
	}
	c.Set(fiber.HeaderCacheControl, "no-store")
	return c.JSON(rows)
}

// Targets answers GET /v1/apps/:clientId/targets?type=group|user&q=.
func (h *AppsHandler) Targets(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return identityError(c, err)
	}
	targets, err := h.svc.AppTargets(c.Context(), p, c.Params("clientId"), c.Query("type"), c.Query("q"))
	if err != nil {
		return appsError(c, err)
	}
	c.Set(fiber.HeaderCacheControl, "no-store")
	return c.JSON(targets)
}
