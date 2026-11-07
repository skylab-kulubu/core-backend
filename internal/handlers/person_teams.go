package handlers

import (
	"context"
	"log"
	"slices"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authn"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/commpref"
	"github.com/skylab-kulubu/core-backend/internal/identity"
)

// PersonTeamsPath is where another service asks which Teams a person is in
// and which they lead (docs/team-delegation.md "A person's Teams").
const PersonTeamsPath = "/internal/v1/users/:sub/teams"

// personTeamsCaller reports whether a service account may ask, each with a
// core client role it already holds: SkyMail with audience:read (Team mail,
// ADR-0067 addendum of 2026-11-08), a role only SkyMail's service account
// holds; Forms with users:read (a form's owner team, ADR-0075 addendum of
// 2026-11-08), and only Forms: users:read is shared (profile reads), so
// another product's service account holding it is refused.
func personTeamsCaller(ident authn.Identity) bool {
	switch {
	case slices.Contains(ident.Roles, commpref.AudienceRole):
		return true
	case slices.Contains(ident.Roles, "users:read"):
		return ident.Product == authz.ProductForms
	}
	return false
}

// PersonTeamsSource answers a person's Teams (identity.TeamDelegation).
type PersonTeamsSource interface {
	PersonTeams(ctx context.Context, userID uuid.UUID) ([]identity.PersonTeam, error)
}

// PersonTeamsHandler serves GET /internal/v1/users/{sub}/teams. Only a
// service account personTeamsCaller allows may call it; the route
// is internal (middlewares.InternalRouteGuard).
type PersonTeamsHandler struct {
	teams PersonTeamsSource
}

// NewPersonTeamsHandler serves teams. A nil source answers 503.
func NewPersonTeamsHandler(teams PersonTeamsSource) *PersonTeamsHandler {
	return &PersonTeamsHandler{teams: teams}
}

// Get is GET /internal/v1/users/{sub}/teams → {"teams": [{"id", "path",
// "code", "displayName": {"tr", "en"?}, "role": "leader"|"member"}]}, in
// path order, whether or not TEAM_DELEGATION is on. No other person, no
// address, no count.
func (h *PersonTeamsHandler) Get(c fiber.Ctx) error {
	c.Set(fiber.HeaderCacheControl, "no-store")
	ident, ok := c.Locals(authn.LocalsIdentity).(authn.Identity)
	if !ok {
		return problem(c, fiber.StatusUnauthorized, "Unauthorized")
	}
	// A person's token is refused whatever it carries; a service account
	// needs one of the roles.
	if !ident.ServiceAccount {
		return problemCode(c, fiber.StatusForbidden, "Forbidden", "person_teams_service_only")
	}
	if !personTeamsCaller(ident) {
		return problemCode(c, fiber.StatusForbidden, "Forbidden", "person_teams_role_missing")
	}
	sub, err := uuid.Parse(c.Params("sub"))
	if err != nil {
		return problemCode(c, fiber.StatusBadRequest, "Bad Request", "person_teams_sub_invalid")
	}
	if h.teams == nil {
		return problemCode(c, fiber.StatusServiceUnavailable, "Service Unavailable", "person_teams_unavailable")
	}
	teams, err := h.teams.PersonTeams(c.Context(), sub)
	if err != nil {
		// The error names a status or what failed, never a person.
		log.Printf("person teams: %v", err)
		c.Set(fiber.HeaderRetryAfter, "30")
		return problemCode(c, fiber.StatusServiceUnavailable, "Service Unavailable", "person_teams_directory_unavailable")
	}
	return c.JSON(fiber.Map{"teams": teams})
}
