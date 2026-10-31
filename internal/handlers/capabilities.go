package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"log"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/requestid"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/editablesites"
	"github.com/skylab-kulubu/core-backend/internal/identity"
	"github.com/skylab-kulubu/core-backend/internal/membershipterm"
)

// EditableSitesSource answers the sites a person may edit in the CMS and
// whether the answer was remembered (editablesites.Sites). A non-nil error
// says some sites could not be read; the list is still the answer.
type EditableSitesSource interface {
	For(ctx context.Context, userID uuid.UUID) ([]editablesites.Site, bool, error)
}

// ManagedTeamsSource answers the Teams a person is a Leader of, read from
// Keycloak (identity.TeamDelegation), and whether the answer was
// remembered. A non-nil error says some could not be read; the list is
// still the answer. Empty with team delegation off.
type ManagedTeamsSource interface {
	ManagedTeams(ctx context.Context, userID uuid.UUID) ([]identity.ManagedTeam, bool, error)
}

// CapabilitiesHandler answers what the caller may do, from the authorizer
// that decides their requests (docs/authz-roles.md), and which sites they
// may edit in the CMS, a hint for the UI that decides nothing.
type CapabilitiesHandler struct {
	az      authz.Authorizer
	sites   EditableSitesSource
	teams   ManagedTeamsSource
	expired ExpiredMembershipsSource
	logger  *log.Logger
}

// ExpiredMembershipsSource answers a person's management memberships whose
// term has expired while they are still in the Group
// (membershipterm.Service, docs/management-membership-terms.md). Empty
// unless the terms are kept (MANAGEMENT_TERMS_MODE=apply).
type ExpiredMembershipsSource interface {
	ExpiredFor(ctx context.Context, userID uuid.UUID) ([]membershipterm.ExpiredMembership, error)
}

// WithExpiredMemberships answers expiredManagementMemberships from source;
// without it every caller's list is empty.
func (h *CapabilitiesHandler) WithExpiredMemberships(source ExpiredMembershipsSource) *CapabilitiesHandler {
	h.expired = source
	return h
}

// NewCapabilitiesHandler answers from az, and the editable sites from sites;
// nil sites answers every caller an empty list.
func NewCapabilitiesHandler(az authz.Authorizer, sites EditableSitesSource) *CapabilitiesHandler {
	return &CapabilitiesHandler{az: az, sites: sites, logger: log.Default()}
}

// WithManagedTeams answers managedTeams from teams; without it every
// caller's list is empty.
func (h *CapabilitiesHandler) WithManagedTeams(teams ManagedTeamsSource) *CapabilitiesHandler {
	h.teams = teams
	return h
}

// capabilitiesAnswer is authz.Capabilities with the editable sites and the
// managed Teams.
type capabilitiesAnswer struct {
	authz.Capabilities
	// ManagedTeams are the Teams the caller is a Leader of, read from
	// Keycloak's current membership with core's service account
	// (docs/team-delegation.md), in path order. A hint for the admin
	// panel's "Ekiplerim" menu: core decides every request. Empty with
	// team delegation off, and when Keycloak cannot be read.
	ManagedTeams []identity.ManagedTeam `json:"managedTeams"`
	// EditableSites are the Site clients the caller holds cms:access on, in
	// the configured order. Only a hint for which "Düzenle" links to show:
	// the CMS decides every edit. Empty when Keycloak cannot be read.
	EditableSites []editablesites.Site `json:"editableSites"`
	// ExpiredManagementMemberships are the caller's YK, DK and Team Leader
	// memberships whose term has expired (they are still in the Group):
	// the panel's banner (grup-yonetimi 11, 12). A hint: the term decides
	// nothing. Empty when the terms are off or cannot be read.
	ExpiredManagementMemberships []membershipterm.ExpiredMembership `json:"expiredManagementMemberships"`
}

// Get answers GET /v1/users/me/capabilities. It is the caller's own answer,
// so no cache keeps it.
func (h *CapabilitiesHandler) Get(c fiber.Ctx) error {
	p, err := caller(c)
	if err != nil {
		if errors.Is(err, fiber.ErrUnauthorized) {
			return problem(c, fiber.StatusUnauthorized, "Unauthorized")
		}
		return err
	}
	c.Set(fiber.HeaderCacheControl, "no-store")
	return c.JSON(capabilitiesAnswer{
		Capabilities: h.az.Capabilities(p), EditableSites: h.editableSites(c, p), ManagedTeams: h.managedTeams(c, p),
		ExpiredManagementMemberships: h.expiredMemberships(c, p),
	})
}

// expiredMemberships never fails the answer: a read that fails answers an
// empty list and is logged, as editableSites is.
func (h *CapabilitiesHandler) expiredMemberships(c fiber.Ctx, p authz.Principal) []membershipterm.ExpiredMembership {
	out := []membershipterm.ExpiredMembership{}
	if h.expired == nil || p.ServiceAccount || p.Product != "" {
		return out
	}
	userID, err := uuid.Parse(p.ID)
	if err != nil {
		return out
	}
	expired, err := h.expired.ExpiredFor(c.Context(), userID)
	if err != nil {
		if h.logger != nil {
			h.logger.Printf(`{"event":"expired_management_memberships","correlation_id":%q,"outcome":"unavailable"}`, requestid.FromContext(c))
		}
		return out
	}
	if expired != nil {
		out = expired
	}
	return out
}

// managedTeams never fails the answer: Teams that cannot be read are left
// out and the read is logged, as editableSites is. A service account
// leads no Team.
func (h *CapabilitiesHandler) managedTeams(c fiber.Ctx, p authz.Principal) []identity.ManagedTeam {
	if h.teams == nil || p.ServiceAccount || p.Product != "" {
		return []identity.ManagedTeam{}
	}
	userID, err := uuid.Parse(p.ID)
	if err != nil {
		return []identity.ManagedTeam{}
	}
	teams, cached, err := h.teams.ManagedTeams(c.Context(), userID)
	if teams == nil {
		teams = []identity.ManagedTeam{}
	}
	if err != nil || !cached {
		h.logRead(c, "managed_teams", "teams", len(teams), err)
	}
	return teams
}

// editableSites never fails the answer: a site that cannot be read is left
// out and logged. A service account edits no site.
func (h *CapabilitiesHandler) editableSites(c fiber.Ctx, p authz.Principal) []editablesites.Site {
	if h.sites == nil || p.ServiceAccount {
		return []editablesites.Site{}
	}
	userID, err := uuid.Parse(p.ID)
	if err != nil {
		return []editablesites.Site{}
	}
	sites, cached, err := h.sites.For(c.Context(), userID)
	if sites == nil {
		sites = []editablesites.Site{}
	}
	if err != nil || !cached {
		h.logRead(c, "editable_sites", "sites", len(sites), err)
	}
	return sites
}

// logRead writes one JSON line per read of a person's sites or Teams: the
// event, the request's correlation id, the outcome and how many came back
// (under countKey). The person and what came back never appear; err names
// nobody.
func (h *CapabilitiesHandler) logRead(c fiber.Ctx, eventName, countKey string, count int, err error) {
	if h.logger == nil {
		return
	}
	event := struct {
		Event         string `json:"event"`
		CorrelationID string `json:"correlation_id"`
		Outcome       string `json:"outcome"`
		Sites         *int   `json:"sites,omitempty"`
		Teams         *int   `json:"teams,omitempty"`
		Error         string `json:"error,omitempty"`
	}{Event: eventName, CorrelationID: requestid.FromContext(c), Outcome: "fetched"}
	if countKey == "teams" {
		event.Teams = &count
	} else {
		event.Sites = &count
	}
	if err != nil {
		event.Outcome = "unavailable"
		event.Error = err.Error()
	}
	if payload, marshalErr := json.Marshal(event); marshalErr == nil {
		h.logger.Print(string(payload))
	}
}
