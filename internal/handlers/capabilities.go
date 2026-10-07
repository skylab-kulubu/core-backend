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
)

// EditableSitesSource answers the sites a person may edit in the CMS and
// whether the answer was remembered (editablesites.Sites). A non-nil error
// says some sites could not be read; the list is still the answer.
type EditableSitesSource interface {
	For(ctx context.Context, userID uuid.UUID) ([]editablesites.Site, bool, error)
}

// CapabilitiesHandler answers what the caller may do, from the authorizer
// that decides their requests (docs/authz-roles.md), and which sites they
// may edit in the CMS, a hint for the UI that decides nothing.
type CapabilitiesHandler struct {
	az     authz.Authorizer
	sites  EditableSitesSource
	logger *log.Logger
}

// NewCapabilitiesHandler answers from az, and the editable sites from sites;
// nil sites answers every caller an empty list.
func NewCapabilitiesHandler(az authz.Authorizer, sites EditableSitesSource) *CapabilitiesHandler {
	return &CapabilitiesHandler{az: az, sites: sites, logger: log.Default()}
}

// capabilitiesAnswer is authz.Capabilities with the editable sites.
type capabilitiesAnswer struct {
	authz.Capabilities
	// EditableSites are the Site clients the caller holds cms:access on, in
	// the configured order. Only a hint for which "Düzenle" links to show:
	// the CMS decides every edit. Empty when Keycloak cannot be read.
	EditableSites []editablesites.Site `json:"editableSites"`
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
	return c.JSON(capabilitiesAnswer{Capabilities: h.az.Capabilities(p), EditableSites: h.editableSites(c, p)})
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
		h.logEditableSites(c, len(sites), err)
	}
	return sites
}

// logEditableSites writes one JSON line per read of a person's sites: the
// request's correlation id, the outcome and how many sites came back. The
// person and their sites never appear; err names nobody.
func (h *CapabilitiesHandler) logEditableSites(c fiber.Ctx, sites int, err error) {
	if h.logger == nil {
		return
	}
	event := struct {
		Event         string `json:"event"`
		CorrelationID string `json:"correlation_id"`
		Outcome       string `json:"outcome"`
		Sites         int    `json:"sites"`
		Error         string `json:"error,omitempty"`
	}{Event: "editable_sites", CorrelationID: requestid.FromContext(c), Outcome: "fetched", Sites: sites}
	if err != nil {
		event.Outcome = "unavailable"
		event.Error = err.Error()
	}
	if payload, marshalErr := json.Marshal(event); marshalErr == nil {
		h.logger.Print(string(payload))
	}
}
