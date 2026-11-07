package handlers

import (
	"bytes"
	"encoding/json"
	"errors"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/limiter"
	"github.com/gofiber/fiber/v3/middleware/requestid"
	"github.com/skylab-kulubu/core-backend/internal/apppref"
	"github.com/skylab-kulubu/core-backend/internal/authn"
)

// AppPreferencePath is the routes' root (docs/app-preferences.md).
const AppPreferencePath = "/v1/users/me/app-preferences"

// AppPreferenceHandler serves App preferences (docs/app-preferences.md, ADR
// 0081): an app's own namespace for the signed-in person, and Account
// center's summary and reset.
type AppPreferenceHandler struct {
	svc *apppref.Service
}

func NewAppPreferenceHandler(svc *apppref.Service) *AppPreferenceHandler {
	return &AppPreferenceHandler{svc: svc}
}

type appPreferenceItem struct {
	Value         json.RawMessage `json:"value"`
	SchemaVersion int             `json:"schemaVersion"`
	Version       int64           `json:"version"`
	UpdatedAt     time.Time       `json:"updatedAt"`
}

type appPreferenceSummary struct {
	App       string    `json:"app"`
	Keys      int       `json:"keys"`
	UpdatedAt time.Time `json:"updatedAt"`
}

type appPreferenceBody struct {
	Value         json.RawMessage `json:"value"`
	SchemaVersion *int            `json:"schemaVersion"`
}

func appPreferenceCaller(c fiber.Ctx) (apppref.Caller, bool) {
	ident, ok := c.Locals(authn.LocalsIdentity).(authn.Identity)
	if !ok {
		return apppref.Caller{}, false
	}
	return apppref.Caller{Person: ident.ID, Client: ident.Client, ServiceAccount: ident.ServiceAccount}, true
}

// appPreferenceError answers err. A refusal of the client and an unexpected
// error are logged with the request id and the app only: never the person
// or a value.
func appPreferenceError(c fiber.Ctx, app string, err error) error {
	switch {
	case errors.Is(err, apppref.ErrOff):
		return problem(c, fiber.StatusNotFound, "Not Found")
	case errors.Is(err, apppref.ErrUnknownApp):
		return problemCode(c, fiber.StatusNotFound, "Not Found", "app_preference_unknown_app")
	case errors.Is(err, apppref.ErrPersonOnly):
		logAppPreference(c, app, "app_preference_person_only")
		return problemCode(c, fiber.StatusForbidden, "Forbidden", "app_preference_person_only")
	case errors.Is(err, apppref.ErrWrongClient):
		logAppPreference(c, app, "app_preference_wrong_client")
		return problemCode(c, fiber.StatusForbidden, "Forbidden", "app_preference_wrong_client")
	case errors.Is(err, apppref.ErrInvalidKey):
		return problemCode(c, fiber.StatusUnprocessableEntity, "Unprocessable Entity", "app_preference_invalid_key")
	case errors.Is(err, apppref.ErrInvalidValue):
		return problemCode(c, fiber.StatusBadRequest, "Bad Request", "app_preference_invalid")
	case errors.Is(err, apppref.ErrValueTooLarge):
		return problemCode(c, fiber.StatusUnprocessableEntity, "Unprocessable Entity", "app_preference_value_too_large")
	case errors.Is(err, apppref.ErrQuota):
		return problemCode(c, fiber.StatusUnprocessableEntity, "Unprocessable Entity", "app_preference_quota_exceeded")
	case errors.Is(err, apppref.ErrConflict):
		return problemCode(c, fiber.StatusPreconditionFailed, "Precondition Failed", "app_preference_conflict")
	case errors.Is(err, apppref.ErrSubjectInactive):
		return problemCode(c, fiber.StatusForbidden, "Forbidden", "app_preference_subject_inactive")
	}
	log.Printf(`{"event":"app_preference_error","correlation_id":%q,"app":%q,"error":%q}`, requestid.FromContext(c), app, err.Error())
	return problem(c, fiber.StatusInternalServerError, "Internal Server Error")
}

func logAppPreference(c fiber.Ctx, app, code string) {
	log.Printf(`{"event":"app_preference_refused","correlation_id":%q,"app":%q,"code":%q}`, requestid.FromContext(c), app, code)
}

// begin sets the answer's cache header and reads the caller: no store
// anywhere between the app and core, the app keeps its own cache.
func (h *AppPreferenceHandler) begin(c fiber.Ctx) (apppref.Caller, bool) {
	c.Set(fiber.HeaderCacheControl, "private, no-store")
	return appPreferenceCaller(c)
}

// Get is GET /v1/users/me/app-preferences/:app: every key the person has in
// the app, {"app", "items": {key: {value, schemaVersion, version,
// updatedAt}}}. None is "items": {}, not 404.
func (h *AppPreferenceHandler) Get(c fiber.Ctx) error {
	caller, ok := h.begin(c)
	if !ok {
		return problem(c, fiber.StatusUnauthorized, "Unauthorized")
	}
	app := c.Params("app")
	items, err := h.svc.Get(c.Context(), caller, app)
	if err != nil {
		return appPreferenceError(c, app, err)
	}
	out := make(map[string]appPreferenceItem, len(items))
	for _, item := range items {
		out[item.Key] = appPreferenceItem{Value: item.Value, SchemaVersion: item.SchemaVersion, Version: item.Version, UpdatedAt: item.UpdatedAt}
	}
	return c.JSON(fiber.Map{"app": app, "items": out})
}

// parseIfMatch reads If-Match: absent is nil, "*" any stored version, a
// quoted version that one. Anything else matches nothing.
func parseIfMatch(header string) *apppref.Match {
	header = strings.TrimSpace(header)
	switch {
	case header == "":
		return nil
	case header == "*":
		return &apppref.Match{Any: true}
	}
	version, err := strconv.ParseInt(strings.Trim(header, `"`), 10, 64)
	if err != nil || version < 1 {
		return &apppref.Match{Version: -1}
	}
	return &apppref.Match{Version: version}
}

// Put is PUT /v1/users/me/app-preferences/:app/:key: {"value": <JSON>,
// "schemaVersion": 1} makes or replaces the key. It answers the stored item
// with its key and an ETag of its version; an If-Match that does not hold
// the stored version is 412.
func (h *AppPreferenceHandler) Put(c fiber.Ctx) error {
	caller, ok := h.begin(c)
	if !ok {
		return problem(c, fiber.StatusUnauthorized, "Unauthorized")
	}
	app, key := c.Params("app"), c.Params("key")
	if len(c.Body()) > apppref.MaxBodyBytes {
		h.svc.Metrics().Record(app, apppref.OpPut, "too_large")
		return problemCode(c, fiber.StatusRequestEntityTooLarge, "Payload Too Large", "app_preference_body_too_large")
	}
	var body appPreferenceBody
	decoder := json.NewDecoder(bytes.NewReader(c.Body()))
	decoder.DisallowUnknownFields()
	schemaVersion := 0
	if err := decoder.Decode(&body); err != nil || decoder.More() {
		body = appPreferenceBody{}
	} else if body.SchemaVersion != nil {
		schemaVersion = *body.SchemaVersion
		if schemaVersion == 0 {
			// An explicit 0 is out of range, not the default.
			schemaVersion = -1
		}
	}
	item, err := h.svc.Put(c.Context(), caller, app, key, body.Value, schemaVersion, parseIfMatch(c.Get(fiber.HeaderIfMatch)))
	if err != nil {
		return appPreferenceError(c, app, err)
	}
	c.Set(fiber.HeaderETag, `"`+strconv.FormatInt(item.Version, 10)+`"`)
	return c.JSON(fiber.Map{
		"app": app, "key": item.Key, "value": item.Value, "schemaVersion": item.SchemaVersion,
		"version": item.Version, "updatedAt": item.UpdatedAt,
	})
}

// DeleteKey is DELETE /v1/users/me/app-preferences/:app/:key: the app's
// default is back. A missing key is 204 too.
func (h *AppPreferenceHandler) DeleteKey(c fiber.Ctx) error {
	caller, ok := h.begin(c)
	if !ok {
		return problem(c, fiber.StatusUnauthorized, "Unauthorized")
	}
	app := c.Params("app")
	if err := h.svc.DeleteKey(c.Context(), caller, app, c.Params("key")); err != nil {
		return appPreferenceError(c, app, err)
	}
	return c.SendStatus(fiber.StatusNoContent)
}

// Reset is DELETE /v1/users/me/app-preferences/:app: every key of the app
// goes. The app's own client and Account center may.
func (h *AppPreferenceHandler) Reset(c fiber.Ctx) error {
	caller, ok := h.begin(c)
	if !ok {
		return problem(c, fiber.StatusUnauthorized, "Unauthorized")
	}
	app := c.Params("app")
	if err := h.svc.Reset(c.Context(), caller, app); err != nil {
		return appPreferenceError(c, app, err)
	}
	return c.SendStatus(fiber.StatusNoContent)
}

// Summary is GET /v1/users/me/app-preferences, Account center's only:
// {"items": [{"app", "keys", "updatedAt"}]} for each app that holds a
// preference. No value.
func (h *AppPreferenceHandler) Summary(c fiber.Ctx) error {
	caller, ok := h.begin(c)
	if !ok {
		return problem(c, fiber.StatusUnauthorized, "Unauthorized")
	}
	summaries, err := h.svc.Summaries(c.Context(), caller)
	if err != nil {
		return appPreferenceError(c, "", err)
	}
	out := make([]appPreferenceSummary, 0, len(summaries))
	for _, sum := range summaries {
		out = append(out, appPreferenceSummary{App: sum.App, Keys: sum.Keys, UpdatedAt: sum.UpdatedAt})
	}
	return c.JSON(fiber.Map{"items": out})
}

// WriteLimit is each person's write budget, apppref.WritesPerMinute a
// minute across PUT and DELETE, kept by each replica.
func (h *AppPreferenceHandler) WriteLimit() fiber.Handler {
	return limiter.New(limiter.Config{
		Max:        apppref.WritesPerMinute,
		Expiration: time.Minute,
		Next: func(c fiber.Ctx) bool {
			_, ok := c.Locals(authn.LocalsIdentity).(authn.Identity)
			return !ok
		},
		KeyGenerator: func(c fiber.Ctx) string {
			ident, _ := c.Locals(authn.LocalsIdentity).(authn.Identity)
			return "app-preferences:" + ident.ID.String()
		},
		LimitReached: func(c fiber.Ctx) error {
			op := apppref.OpPut
			switch {
			case c.Method() == fiber.MethodDelete && c.Params("key") == "":
				op = apppref.OpReset
			case c.Method() == fiber.MethodDelete:
				op = apppref.OpDelete
			}
			h.svc.Metrics().Record(c.Params("app"), op, "rate_limited")
			c.Set(fiber.HeaderCacheControl, "private, no-store")
			seconds, _ := strconv.Atoi(string(c.Response().Header.Peek(fiber.HeaderRetryAfter)))
			return problemWithFields(c, fiber.StatusTooManyRequests, "Too Many Requests",
				"Too many requests; retry after the given seconds.", "app_preference_rate_limited", fiber.Map{"retryAfterSeconds": seconds})
		},
	})
}
