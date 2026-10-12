package handlers

import (
	"bytes"
	"encoding/json"
	"errors"
	"log"
	"slices"
	"strconv"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authn"
	"github.com/skylab-kulubu/core-backend/internal/commpref"
)

// AudienceHandler serves POST /v1/audiences/resolve: who a News send goes to
// (docs/news-audience.md). Only a service account holding
// commpref.AudienceRole (SkyMail's) may call it.
type AudienceHandler struct {
	svc *commpref.Audience
}

// NewAudienceHandler serves svc. A nil or disabled service answers 503.
func NewAudienceHandler(svc *commpref.Audience) *AudienceHandler {
	return &AudienceHandler{svc: svc}
}

func audienceError(c fiber.Ctx, err error) error {
	c.Set(fiber.HeaderCacheControl, "no-store")
	switch {
	case errors.Is(err, commpref.ErrDisabled):
		return problemCode(c, fiber.StatusServiceUnavailable, "Service Unavailable", "audience_unavailable")
	case errors.Is(err, commpref.ErrTeamsUnavailable):
		// The error names a status or what failed, never a person.
		log.Printf("news audience: %v", err)
		c.Set(fiber.HeaderRetryAfter, "30")
		return problemCode(c, fiber.StatusServiceUnavailable, "Service Unavailable", "audience_directory_unavailable")
	case errors.Is(err, commpref.ErrUnknownTeam):
		return problemCode(c, fiber.StatusBadRequest, "Bad Request", "audience_team_unknown")
	case errors.Is(err, commpref.ErrVisibilityNotEnabled):
		return problemCode(c, fiber.StatusBadRequest, "Bad Request", "audience_visibility_not_enabled")
	case errors.Is(err, commpref.ErrInvalid):
		return problemCode(c, fiber.StatusBadRequest, "Bad Request", "audience_invalid")
	}
	log.Printf("news audience: %v", err)
	return problem(c, fiber.StatusInternalServerError, "Internal Server Error")
}

type audienceBody struct {
	Type            string   `json:"type"`
	Teams           []string `json:"teams"`
	NewsKind        string   `json:"newsKind"`
	Visibility      string   `json:"visibility"`
	VisibilityTeams []string `json:"visibilityTeams"`
}

// Resolve is POST /v1/audiences/resolve[?countOnly=true][&after=<userId>][&limit=1..500].
func (h *AudienceHandler) Resolve(c fiber.Ctx) error {
	ident, ok := c.Locals(authn.LocalsIdentity).(authn.Identity)
	if !ok {
		return problem(c, fiber.StatusUnauthorized, "Unauthorized")
	}
	// A person's token is refused whatever it carries; a service account
	// needs the role.
	if !ident.ServiceAccount {
		return problemCode(c, fiber.StatusForbidden, "Forbidden", "audience_service_only")
	}
	if !slices.Contains(ident.Roles, commpref.AudienceRole) {
		return problemCode(c, fiber.StatusForbidden, "Forbidden", "audience_role_missing")
	}
	if !h.svc.Enabled() {
		return audienceError(c, commpref.ErrDisabled)
	}
	var body audienceBody
	decoder := json.NewDecoder(bytes.NewReader(c.Body()))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil {
		return audienceError(c, commpref.ErrInvalid)
	}
	countOnly := false
	switch c.Query("countOnly") {
	case "", "false":
	case "true":
		countOnly = true
	default:
		return audienceError(c, commpref.ErrInvalid)
	}
	after := uuid.Nil
	if raw := c.Query("after"); raw != "" {
		parsed, err := uuid.Parse(raw)
		if err != nil {
			return audienceError(c, commpref.ErrInvalid)
		}
		after = parsed
	}
	limit := commpref.MaxAudiencePage
	if raw := c.Query("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > commpref.MaxAudiencePage {
			return audienceError(c, commpref.ErrInvalid)
		}
		limit = parsed
	}
	req := commpref.AudienceRequest{
		Type:            commpref.AudienceType(body.Type),
		Teams:           body.Teams,
		Kind:            commpref.Kind(body.NewsKind),
		Visibility:      commpref.Visibility(body.Visibility),
		VisibilityTeams: body.VisibilityTeams,
	}
	page, err := h.svc.Resolve(c.Context(), req, after, limit, countOnly)
	if err != nil {
		return audienceError(c, err)
	}
	// Counts only: the answer carries addresses and names, the log never.
	s := page.Skipped
	log.Printf(`{"event":"news_audience","client":%q,"type":%q,"teams":%d,"kind":%q,"visibility":%q,"count_only":%t,"page":%d,"total":%d,"members":%d,"followers":%d,"not_member":%d,"not_visible":%d,"disabled":%d,"no_email":%d,"unverified":%d,"opted_out":%d,"duplicate_email":%d}`,
		ident.Client, body.Type, len(body.Teams), body.NewsKind, body.Visibility, countOnly, len(page.Items),
		page.Counts.Total, page.Counts.Members, page.Counts.Followers,
		s.NotMember, s.NotVisible, s.Disabled, s.NoEmail, s.Unverified, s.OptedOut, s.DuplicateEmail)
	c.Set(fiber.HeaderCacheControl, "no-store")
	answer := fiber.Map{"counts": page.Counts, "skipped": page.Skipped}
	if !countOnly {
		items := page.Items
		if items == nil {
			items = []commpref.Recipient{}
		}
		answer["items"] = items
		answer["next"] = nil
		if page.Next != uuid.Nil {
			answer["next"] = page.Next.String()
		}
	}
	return c.JSON(answer)
}
