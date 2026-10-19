package handlers

import (
	"bytes"
	"encoding/json"
	"errors"
	"log"
	"slices"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authn"
	"github.com/skylab-kulubu/core-backend/internal/commpref"
)

// TrackingResolvePath is where SkyMail asks whom a send may track (hub
// ticket 16, skymail-backend README "core sözleşmesi"). The colon is
// literal; Fiber reads an unescaped one as a parameter.
const TrackingResolvePath = "/internal/v1/communication-preferences/tracking\\:resolve"

// TrackingResolveHandler serves POST
// /internal/v1/communication-preferences/tracking:resolve. Only a service
// account holding commpref.TrackingResolveRole (SkyMail's) may call it; the
// route is internal (middlewares.InternalRouteGuard) and needs no
// COMMUNICATION_PREFERENCE_KEY.
type TrackingResolveHandler struct {
	svc *commpref.Audience
}

// NewTrackingResolveHandler serves svc. A nil service answers 503.
func NewTrackingResolveHandler(svc *commpref.Audience) *TrackingResolveHandler {
	return &TrackingResolveHandler{svc: svc}
}

func trackingResolveError(c fiber.Ctx, err error) error {
	c.Set(fiber.HeaderCacheControl, "no-store")
	switch {
	case errors.Is(err, commpref.ErrDisabled):
		return problemCode(c, fiber.StatusServiceUnavailable, "Service Unavailable", "tracking_resolve_unavailable")
	case errors.Is(err, commpref.ErrTeamsUnavailable):
		// The error names a status or what failed, never a person.
		log.Printf("tracking resolve: %v", err)
		c.Set(fiber.HeaderRetryAfter, "30")
		return problemCode(c, fiber.StatusServiceUnavailable, "Service Unavailable", "tracking_resolve_directory_unavailable")
	case errors.Is(err, commpref.ErrInvalid):
		return problemCode(c, fiber.StatusBadRequest, "Bad Request", "tracking_resolve_invalid")
	}
	log.Printf("tracking resolve: %v", err)
	return problem(c, fiber.StatusInternalServerError, "Internal Server Error")
}

type trackingResolveBody struct {
	Subs *[]string `json:"subs"`
}

type trackingResolveResult struct {
	Sub     string  `json:"sub"`
	Allowed bool    `json:"allowed"`
	Reason  *string `json:"reason"`
}

// Resolve is POST /internal/v1/communication-preferences/tracking:resolve
// {"subs": [≤1000 lower-case UUIDs, each once]} → {"results": [{"sub",
// "allowed", "reason": "preference_off"|"not_member"|"inactive"|null}]},
// every sub asked exactly once, in the order asked.
func (h *TrackingResolveHandler) Resolve(c fiber.Ctx) error {
	ident, ok := c.Locals(authn.LocalsIdentity).(authn.Identity)
	if !ok {
		return problem(c, fiber.StatusUnauthorized, "Unauthorized")
	}
	// A person's token is refused whatever it carries; a service account
	// needs the role.
	if !ident.ServiceAccount {
		return problemCode(c, fiber.StatusForbidden, "Forbidden", "tracking_resolve_service_only")
	}
	if !slices.Contains(ident.Roles, commpref.TrackingResolveRole) {
		return problemCode(c, fiber.StatusForbidden, "Forbidden", "tracking_resolve_role_missing")
	}
	if !h.svc.TrackingAvailable() {
		return trackingResolveError(c, commpref.ErrDisabled)
	}
	var body trackingResolveBody
	decoder := json.NewDecoder(bytes.NewReader(c.Body()))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil || body.Subs == nil {
		return trackingResolveError(c, commpref.ErrInvalid)
	}
	raw := *body.Subs
	if len(raw) > commpref.MaxTrackingSubjects {
		return trackingResolveError(c, commpref.ErrInvalid)
	}
	subjects := make([]uuid.UUID, 0, len(raw))
	for _, sub := range raw {
		// Keycloak subjects are lower-case hyphenated UUIDs; another
		// spelling is refused, not matched.
		parsed, err := uuid.Parse(sub)
		if err != nil || parsed.String() != sub {
			return trackingResolveError(c, commpref.ErrInvalid)
		}
		subjects = append(subjects, parsed)
	}
	decisions, err := h.svc.ResolveTracking(c.Context(), subjects)
	if err != nil {
		return trackingResolveError(c, err)
	}
	results := make([]trackingResolveResult, 0, len(decisions))
	counts := map[commpref.TrackingReason]int{}
	allowed := 0
	for _, d := range decisions {
		r := trackingResolveResult{Sub: d.Subject.String(), Allowed: d.Allowed}
		if d.Allowed {
			allowed++
		} else {
			reason := string(d.Reason)
			r.Reason = &reason
			counts[d.Reason]++
		}
		results = append(results, r)
	}
	// Counts only: never a subject.
	log.Printf(`{"event":"tracking_resolve","client":%q,"mode":%q,"asked":%d,"allowed":%d,"preference_off":%d,"not_member":%d,"inactive":%d}`,
		ident.Client, h.svc.TrackingMode(), len(decisions), allowed,
		counts[commpref.TrackingPreferenceOff], counts[commpref.TrackingNotMember], counts[commpref.TrackingInactive])
	c.Set(fiber.HeaderCacheControl, "no-store")
	return c.JSON(fiber.Map{"results": results})
}
