package handlers

import (
	"context"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authn"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/feedback"
)

// reminders records what the handler asked.
type reminders struct {
	cancelled []feedback.Target
	states    []formState
}

type formState struct {
	form     uuid.UUID
	open     bool
	closesAt *time.Time
}

func (r *reminders) CancelReminders(_ context.Context, t feedback.Target, _ time.Time) (int, error) {
	r.cancelled = append(r.cancelled, t)
	return 2, nil
}

func (r *reminders) ReportFormState(_ context.Context, form uuid.UUID, open bool, closesAt *time.Time, _ time.Time) (int, error) {
	r.states = append(r.states, formState{form, open, closesAt})
	return 0, nil
}

var reminderForms = authn.Identity{ID: uuid.New(), Client: "forms", ServiceAccount: true, Product: authz.ProductForms, Roles: []string{"ticket:forms"}}

func reminderApp(e *draftEnv, rem *reminders, ident authn.Identity) *fiber.App {
	evSvc := event.NewService(e.events, authz.NewAuthorizer(authz.DefaultPolicy()))
	h := NewFeedbackHandler(evSvc, authz.NewAuthorizer(authz.DefaultPolicy()), fixedCounts{}).RemindWith(rem)
	a := fiber.New()
	a.Use(func(c fiber.Ctx) error {
		if ident.ID != uuid.Nil || len(ident.Groups) > 0 {
			c.Locals(authn.LocalsIdentity, ident)
		}
		return c.Next()
	})
	a.Post("/v1/events/:id/feedback/close", h.CloseEvent)
	a.Post("/v1/sessions/:id/feedback/close", h.CloseSession)
	a.Put("/v1/forms/:formId/state", h.FormState)
	return a
}

// The panel's switch (ticket 07): whoever may edit the Event cancels the
// reminders of its form or of a Session's.
func TestFeedbackCloseEndpoints(t *testing.T) {
	t.Parallel()
	e := newDraftEnv(t)
	rem := &reminders{}
	pub := "/v1/events/" + e.published.ID.String() + "/feedback/close"
	for _, tc := range []struct {
		name   string
		ident  authn.Identity
		status int
	}{
		{"signed out", draftAnon, fiber.StatusUnauthorized},
		{"member", draftMember, fiber.StatusForbidden},
		{"DK", draftDK, fiber.StatusForbidden},
		{"the forms service", reminderForms, fiber.StatusForbidden},
		{"editor", draftEditor, fiber.StatusOK},
	} {
		got := draftCall(t, reminderApp(e, rem, tc.ident), fiber.MethodPost, pub, "")
		if got.status != tc.status {
			t.Errorf("%s: %d %s", tc.name, got.status, got.raw)
		}
		if tc.status == fiber.StatusOK && got.body["cancelled"] != float64(2) {
			t.Errorf("%s: %s", tc.name, got.raw)
		}
	}
	got := draftCall(t, reminderApp(e, rem, draftEditor), fiber.MethodPost, "/v1/sessions/"+e.pubSession.ID.String()+"/feedback/close", "")
	if got.status != fiber.StatusOK || len(rem.cancelled) != 2 || rem.cancelled[0].SessionID != nil ||
		rem.cancelled[1].SessionID == nil || *rem.cancelled[1].SessionID != e.pubSession.ID || rem.cancelled[1].EventID != e.published.ID {
		t.Fatalf("session %d %s %+v", got.status, got.raw, rem.cancelled)
	}
	if got := draftCall(t, reminderApp(e, rem, draftOtherLeader), fiber.MethodPost, "/v1/events/"+e.draft.ID.String()+"/feedback/close", ""); got.status != fiber.StatusNotFound {
		t.Fatalf("another team's draft %d", got.status)
	}
}

// The forms service's report (ticket 07, S3): only it may; open is
// required, closesAt an RFC 3339 time with its zone or null; a closed form
// takes no closing time.
func TestFeedbackFormStateEndpoint(t *testing.T) {
	t.Parallel()
	e := newDraftEnv(t)
	rem := &reminders{}
	form := uuid.New()
	path := "/v1/forms/" + form.String() + "/state"
	for _, tc := range []struct {
		name   string
		ident  authn.Identity
		body   string
		status int
	}{
		{"signed out", draftAnon, `{"open":false}`, fiber.StatusUnauthorized},
		{"an editor", draftEditor, `{"open":false}`, fiber.StatusForbidden},
		{"DK", draftDK, `{"open":false}`, fiber.StatusForbidden},
		{"an editor, malformed", draftEditor, `{`, fiber.StatusForbidden},
		{"no open", reminderForms, `{"closesAt":null}`, fiber.StatusBadRequest},
		{"a time without a zone", reminderForms, `{"open":true,"closesAt":"2026-11-10T18:00:00"}`, fiber.StatusBadRequest},
		{"closed", reminderForms, `{"open":false,"closesAt":"2026-11-10T18:00:00Z"}`, fiber.StatusNoContent},
		{"closing", reminderForms, `{"open":true,"closesAt":"2026-11-10T21:00:00+03:00"}`, fiber.StatusNoContent},
		{"open", reminderForms, `{"open":true,"closesAt":null}`, fiber.StatusNoContent},
	} {
		if got := draftCall(t, reminderApp(e, rem, tc.ident), fiber.MethodPut, path, tc.body); got.status != tc.status {
			t.Errorf("%s: %d %s", tc.name, got.status, got.raw)
		}
	}
	if got := draftCall(t, reminderApp(e, rem, reminderForms), fiber.MethodPut, "/v1/forms/x/state", `{"open":false}`); got.status != fiber.StatusBadRequest {
		t.Errorf("not a form id: %d", got.status)
	}
	closing := time.Date(2026, 11, 10, 18, 0, 0, 0, time.UTC)
	if len(rem.states) != 3 || rem.states[0] != (formState{form, false, nil}) ||
		rem.states[1].form != form || !rem.states[1].open || rem.states[1].closesAt == nil || !rem.states[1].closesAt.Equal(closing) ||
		rem.states[2] != (formState{form, true, nil}) {
		t.Fatalf("states %+v", rem.states)
	}
}
