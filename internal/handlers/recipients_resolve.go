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

// RecipientsResolvePath is where SkyMail asks, for the recipients a sender
// chose, which are a person and whether the mail may go to each (ADR 0072
// karar 3, docs/news-audience.md "Recipients"). The colon is literal; Fiber
// reads an unescaped one as a parameter.
const RecipientsResolvePath = "/internal/v1/audiences/recipients\\:resolve"

// RecipientsResolveHandler serves POST
// /internal/v1/audiences/recipients:resolve. Only a service account holding
// commpref.AudienceRole (SkyMail's) may call it; the route is internal
// (middlewares.InternalRouteGuard).
type RecipientsResolveHandler struct {
	svc *commpref.Recipients
}

// NewRecipientsResolveHandler serves svc. A nil or disabled service answers
// 503.
func NewRecipientsResolveHandler(svc *commpref.Recipients) *RecipientsResolveHandler {
	return &RecipientsResolveHandler{svc: svc}
}

func recipientsResolveError(c fiber.Ctx, err error) error {
	c.Set(fiber.HeaderCacheControl, "no-store")
	switch {
	case errors.Is(err, commpref.ErrDisabled):
		// No link can be signed without COMMUNICATION_PREFERENCE_KEY.
		return problemCode(c, fiber.StatusServiceUnavailable, "Service Unavailable", "communication_preferences_unavailable")
	case errors.Is(err, commpref.ErrTeamsUnavailable), errors.Is(err, commpref.ErrAddressDirectoryUnavailable):
		// The error names a status or what failed, never a person.
		log.Printf("recipients resolve: %v", err)
		c.Set(fiber.HeaderRetryAfter, "30")
		return problemCode(c, fiber.StatusServiceUnavailable, "Service Unavailable", "recipients_resolve_directory_unavailable")
	case errors.Is(err, commpref.ErrUnknownTeam):
		return problemCode(c, fiber.StatusBadRequest, "Bad Request", "recipients_resolve_category_unknown")
	case errors.Is(err, commpref.ErrInvalid):
		return problemCode(c, fiber.StatusBadRequest, "Bad Request", "recipients_resolve_invalid")
	}
	log.Printf("recipients resolve: %v", err)
	return problem(c, fiber.StatusInternalServerError, "Internal Server Error")
}

type recipientsResolveItem struct {
	Email *string `json:"email"`
	Sub   *string `json:"sub"`
}

type recipientsResolveBody struct {
	Channel    string                   `json:"channel"`
	Category   string                   `json:"category"`
	Recipients *[]recipientsResolveItem `json:"recipients"`
}

// recipientsResolveResult is one answer: {"matched": false} alone for a
// recipient core does not know; "reason" null for an allowed person, as
// tracking:resolve answers it; "sub" left out for an ambiguous address and
// "unsubscribeUrl" for a person the mail may not go to.
type recipientsResolveResult struct {
	Matched        bool
	Sub            *string
	Allowed        bool
	Reason         *string
	UnsubscribeURL string
}

func (r recipientsResolveResult) MarshalJSON() ([]byte, error) {
	if !r.Matched {
		return []byte(`{"matched":false}`), nil
	}
	return json.Marshal(struct {
		Matched        bool    `json:"matched"`
		Sub            *string `json:"sub,omitempty"`
		Allowed        bool    `json:"allowed"`
		Reason         *string `json:"reason"`
		UnsubscribeURL string  `json:"unsubscribeUrl,omitempty"`
	}{r.Matched, r.Sub, r.Allowed, r.Reason, r.UnsubscribeURL})
}

// Resolve is POST /internal/v1/audiences/recipients:resolve
// {"channel":"email","category":"club:duyuru"|"team:<groupId>:haber"|…,
// "recipients":[{"email":…}|{"sub":…}, 1–1000, each once]} → {"results":
// [{"matched":false} | {"matched":true,"sub","allowed","reason",
// "unsubscribeUrl"}]}, in the order asked.
func (h *RecipientsResolveHandler) Resolve(c fiber.Ctx) error {
	ident, ok := c.Locals(authn.LocalsIdentity).(authn.Identity)
	if !ok {
		return problem(c, fiber.StatusUnauthorized, "Unauthorized")
	}
	// A person's token is refused whatever it carries; a service account
	// needs the role.
	if !ident.ServiceAccount {
		return problemCode(c, fiber.StatusForbidden, "Forbidden", "recipients_resolve_service_only")
	}
	if !slices.Contains(ident.Roles, commpref.AudienceRole) {
		return problemCode(c, fiber.StatusForbidden, "Forbidden", "recipients_resolve_role_missing")
	}
	if !h.svc.Enabled() {
		return recipientsResolveError(c, commpref.ErrDisabled)
	}
	var body recipientsResolveBody
	decoder := json.NewDecoder(bytes.NewReader(c.Body()))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil || body.Recipients == nil {
		return recipientsResolveError(c, commpref.ErrInvalid)
	}
	channel, err := commpref.ParseChannel(body.Channel)
	if err != nil {
		return recipientsResolveError(c, err)
	}
	category, err := commpref.ParseCategory(body.Category)
	if err != nil {
		return recipientsResolveError(c, err)
	}
	items := *body.Recipients
	if len(items) == 0 || len(items) > commpref.MaxRecipients {
		return recipientsResolveError(c, commpref.ErrInvalid)
	}
	queries := make([]commpref.RecipientQuery, 0, len(items))
	for _, item := range items {
		switch {
		case item.Email != nil && item.Sub == nil:
			queries = append(queries, commpref.RecipientQuery{Email: *item.Email})
		case item.Sub != nil && item.Email == nil:
			// Keycloak subjects are lower-case hyphenated UUIDs; another
			// spelling is refused, not matched.
			parsed, err := uuid.Parse(*item.Sub)
			if err != nil || parsed.String() != *item.Sub || parsed == uuid.Nil {
				return recipientsResolveError(c, commpref.ErrInvalid)
			}
			queries = append(queries, commpref.RecipientQuery{Sub: parsed})
		default:
			return recipientsResolveError(c, commpref.ErrInvalid)
		}
	}
	decisions, err := h.svc.Resolve(c.Context(), channel, category, queries)
	if err != nil {
		return recipientsResolveError(c, err)
	}
	results := make([]recipientsResolveResult, 0, len(decisions))
	counts := map[commpref.RecipientReason]int{}
	matched, allowed := 0, 0
	for _, d := range decisions {
		if !d.Matched {
			results = append(results, recipientsResolveResult{})
			continue
		}
		matched++
		r := recipientsResolveResult{Matched: true, Allowed: d.Allowed}
		if d.Sub != uuid.Nil {
			sub := d.Sub.String()
			r.Sub = &sub
		}
		if d.Allowed {
			allowed++
			r.UnsubscribeURL = d.UnsubscribeURL
		} else {
			reason := string(d.Reason)
			r.Reason = &reason
			counts[d.Reason]++
		}
		results = append(results, r)
	}
	// Counts only: never an address, a subject or a link.
	log.Printf(`{"event":"recipients_resolve","client":%q,"scope":%q,"kind":%q,"asked":%d,"matched":%d,"allowed":%d,"inactive":%d,"opted_out":%d,"ambiguous":%d}`,
		ident.Client, category.Scope, category.Kind, len(decisions), matched, allowed,
		counts[commpref.RecipientInactive], counts[commpref.RecipientOptedOut], counts[commpref.RecipientAmbiguous])
	c.Set(fiber.HeaderCacheControl, "no-store")
	return c.JSON(fiber.Map{"results": results})
}
