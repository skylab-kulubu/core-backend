package handlers

import (
	"bytes"
	"encoding/json"
	"errors"
	"log"
	"regexp"
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
	case errors.Is(err, commpref.ErrTypeNotEnabled):
		return problemCode(c, fiber.StatusBadRequest, "Bad Request", "audience_type_not_enabled")
	case errors.Is(err, commpref.ErrFormUnknown):
		return problemCode(c, fiber.StatusNotFound, "Not Found", "audience_form_unknown")
	case errors.Is(err, commpref.ErrFormForbidden):
		return problemCode(c, fiber.StatusForbidden, "Forbidden", "audience_form_forbidden")
	case errors.Is(err, commpref.ErrFormRespondentsTooMany):
		return problemCode(c, fiber.StatusUnprocessableEntity, "Unprocessable Entity", "audience_form_too_many")
	case errors.Is(err, commpref.ErrFormsUnavailable):
		// The error names a status or what failed, never a person.
		log.Printf("form respondents audience: %v", err)
		c.Set(fiber.HeaderRetryAfter, "30")
		return problemCode(c, fiber.StatusServiceUnavailable, "Service Unavailable", "audience_forms_unavailable")
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
	// The Form respondents audience's (type form_respondents).
	FormID      *string `json:"formId"`
	ActorSub    *string `json:"actorSub"`
	Purpose     string  `json:"purpose"`
	Category    string  `json:"category"`
	Promotional bool    `json:"promotional"`
}

// newsFields reports whether body carries a News audience's fields.
func (b audienceBody) newsFields() bool {
	return b.Teams != nil || b.NewsKind != "" || b.Visibility != "" || b.VisibilityTeams != nil
}

// formFields reports whether body carries a Form respondents audience's
// fields.
func (b audienceBody) formFields() bool {
	return b.FormID != nil || b.ActorSub != nil || b.Purpose != "" || b.Category != "" || b.Promotional
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
	if commpref.AudienceType(body.Type) == commpref.AudienceFormRespondents {
		if body.newsFields() {
			return audienceError(c, commpref.ErrInvalid)
		}
		return h.resolveForm(c, ident, body, countOnly)
	}
	if body.formFields() {
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

// subjectField reads a Keycloak subject: a lower-case hyphenated UUID; any
// other spelling is refused.
func subjectField(raw *string) (uuid.UUID, bool) {
	if raw == nil {
		return uuid.Nil, false
	}
	parsed, err := uuid.Parse(*raw)
	if err != nil || parsed.String() != *raw || parsed == uuid.Nil {
		return uuid.Nil, false
	}
	return parsed, true
}

// resolveForm answers a form_respondents question (ADR-0075 karar 6,
// docs/news-audience.md "Form respondents"): {"type":"form_respondents",
// "formId","actorSub","purpose":"islem"|"duyuru","category","promotional"}.
// actorSub is the sender SkyMail signed in; its right is checked here, on
// every page.
func (h *AudienceHandler) resolveForm(c fiber.Ctx, ident authn.Identity, body audienceBody, countOnly bool) error {
	if !h.svc.FormRespondentsOn() {
		return audienceError(c, commpref.ErrTypeNotEnabled)
	}
	formID, okForm := subjectField(body.FormID)
	actor, okActor := subjectField(body.ActorSub)
	if !okForm || !okActor {
		return audienceError(c, commpref.ErrInvalid)
	}
	after := c.Query("after")
	if after != "" && !formCursor.MatchString(after) {
		return audienceError(c, commpref.ErrInvalid)
	}
	limit := commpref.MaxAudiencePage
	if raw := c.Query("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > commpref.MaxAudiencePage {
			return audienceError(c, commpref.ErrInvalid)
		}
		limit = parsed
	}
	req := commpref.FormRespondentsRequest{
		FormID: formID, ActorSub: actor, Purpose: commpref.Purpose(body.Purpose),
		Category: body.Category, Promotional: body.Promotional, Client: ident.Client,
	}
	page, err := h.svc.ResolveForm(c.Context(), req, after, limit, countOnly)
	if err != nil {
		return audienceError(c, err)
	}
	// Counts only: never an address, a name or the sender (the audit row
	// keeps who sent).
	s := page.Skipped
	log.Printf(`{"event":"form_respondents_audience","client":%q,"purpose":%q,"promotional":%t,"count_only":%t,"page":%d,"total":%d,"members":%d,"non_members":%d,"guests":%d,"responses":%d,"invalid_email":%d,"disabled":%d,"duplicate_email":%d,"opted_out":%d,"no_consent":%d,"no_promotional_consent":%d}`,
		ident.Client, body.Purpose, body.Promotional, countOnly, len(page.Items),
		page.Counts.Total, page.Counts.Members, page.Counts.NonMembers, page.Counts.Guests, page.Counts.Responses,
		s.InvalidEmail, s.Disabled, s.DuplicateEmail, s.OptedOut, s.NoConsent, s.NoPromotionalConsent)
	c.Set(fiber.HeaderCacheControl, "no-store")
	answer := fiber.Map{"counts": page.Counts, "skipped": page.Skipped}
	if !countOnly {
		items := page.Items
		if items == nil {
			items = []commpref.FormRecipient{}
		}
		answer["items"] = items
		answer["next"] = nil
		if page.Next != "" {
			answer["next"] = page.Next
		}
	}
	return c.JSON(answer)
}

// formCursor is a Form respondents page cursor: 32 lower-case hex digits.
var formCursor = regexp.MustCompile(`^[0-9a-f]{32}$`)
