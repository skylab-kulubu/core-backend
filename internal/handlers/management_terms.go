package handlers

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/identity"
	"github.com/skylab-kulubu/core-backend/internal/membershipterm"
)

// ManagementTerms are the management membership terms' routes
// (docs/management-membership-terms.md): the panel's handover and extend
// screen (grup-yonetimi 12).
type ManagementTerms interface {
	List(ctx context.Context, p authz.Principal, f membershipterm.Filter) ([]membershipterm.Item, error)
	Extend(ctx context.Context, p authz.Principal, id uuid.UUID, endsAt time.Time) (membershipterm.Item, error)
	Handover(ctx context.Context, p authz.Principal, id uuid.UUID, in membershipterm.HandoverInput) (membershipterm.Item, error)
	Remove(ctx context.Context, p authz.Principal, id uuid.UUID) error
}

// ManagementTermsHandler serves the term routes.
type ManagementTermsHandler struct {
	svc ManagementTerms
}

func NewManagementTermsHandler(svc ManagementTerms) *ManagementTermsHandler {
	return &ManagementTermsHandler{svc: svc}
}

// List answers GET /v1/management-memberships?season=&role=&status=.
func (h *ManagementTermsHandler) List(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		return identityError(c, err)
	}
	var f membershipterm.Filter
	if raw := strings.TrimSpace(c.Query("season")); raw != "" {
		id, err := uuid.Parse(raw)
		if err != nil {
			return problem(c, fiber.StatusBadRequest, "Bad Request")
		}
		f.SeasonID = &id
	}
	if f.Role = strings.TrimSpace(c.Query("role")); f.Role != "" && !membershipterm.ValidRole(f.Role) {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	if f.Status = membershipterm.Status(strings.TrimSpace(c.Query("status"))); f.Status != "" && !membershipterm.ValidStatus(f.Status) {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	items, err := h.svc.List(c.Context(), p, f)
	if err != nil {
		return termError(c, err)
	}
	c.Set(fiber.HeaderCacheControl, "no-store")
	return c.JSON(items)
}

type extendTermBody struct {
	// EndsAt is a day, "2027-09-30": the term ends at its end in
	// Istanbul.
	EndsAt string `json:"endsAt"`
}

// Extend answers POST /v1/management-memberships/{termId}/extend.
func (h *ManagementTermsHandler) Extend(c fiber.Ctx) error {
	p, id, err := termRequest(c)
	if err != nil {
		return err
	}
	var body extendTermBody
	if err := c.Bind().Body(&body); err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	endsAt, err := membershipterm.ParseDay(body.EndsAt)
	if err != nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	item, err := h.svc.Extend(auditContext(c, p), p, id, endsAt)
	if err != nil {
		return termError(c, err)
	}
	return c.JSON(item)
}

type handoverTermBody struct {
	UserID uuid.UUID `json:"userId"`
	// Mode is "immediate" or "at_term_end".
	Mode   string `json:"mode"`
	EndsAt string `json:"endsAt"`
	// UnverifiedAck confirms the warning of adding someone not YTÜ-linked
	// to a Privileged Group, as the member route's does.
	UnverifiedAck bool `json:"unverifiedAck"`
}

// Handover answers POST /v1/management-memberships/{termId}/handover with
// the successor's term.
func (h *ManagementTermsHandler) Handover(c fiber.Ctx) error {
	p, id, err := termRequest(c)
	if err != nil {
		return err
	}
	var body handoverTermBody
	if err := c.Bind().Body(&body); err != nil || body.UserID == uuid.Nil {
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	in := membershipterm.HandoverInput{UserID: body.UserID, Mode: membershipterm.HandoverMode(body.Mode)}
	if strings.TrimSpace(body.EndsAt) != "" {
		endsAt, err := membershipterm.ParseDay(body.EndsAt)
		if err != nil {
			return problem(c, fiber.StatusBadRequest, "Bad Request")
		}
		in.EndsAt = &endsAt
	}
	ctx := identity.WithUnverifiedAck(auditContext(c, p), body.UnverifiedAck)
	item, err := h.svc.Handover(ctx, p, id, in)
	if err != nil {
		return termError(c, err)
	}
	return c.JSON(item)
}

// Remove answers DELETE /v1/management-memberships/{termId}: an expired
// term's holder leaves the Group.
func (h *ManagementTermsHandler) Remove(c fiber.Ctx) error {
	p, id, err := termRequest(c)
	if err != nil {
		return err
	}
	if err := h.svc.Remove(auditContext(c, p), p, id); err != nil {
		return termError(c, err)
	}
	return c.SendStatus(fiber.StatusNoContent)
}

// termRequest is the caller and the route's term id; a written error
// answer otherwise.
func termRequest(c fiber.Ctx) (authz.Principal, uuid.UUID, error) {
	p, err := caller(c)
	if err != nil {
		return p, uuid.Nil, identityError(c, err)
	}
	id, err := uuid.Parse(c.Params("termId"))
	if err != nil {
		return p, uuid.Nil, problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	return p, id, nil
}

func termError(c fiber.Ctx, err error) error {
	var refused *membershipterm.RefusalError
	switch {
	case errors.As(err, &refused):
		title := delegationTitles[refused.Status]
		if title == "" {
			title = "Unprocessable Entity"
		}
		return problemCode(c, refused.Status, title, refused.Code)
	case errors.Is(err, membershipterm.ErrOff), errors.Is(err, membershipterm.ErrNotFound):
		return problem(c, fiber.StatusNotFound, "Not Found")
	case errors.Is(err, membershipterm.ErrInvalid):
		return problem(c, fiber.StatusBadRequest, "Bad Request")
	}
	return identityError(c, err)
}
