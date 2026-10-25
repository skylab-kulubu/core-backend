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
}

func (r *reminders) CancelReminders(_ context.Context, t feedback.Target, _ time.Time) (int, error) {
	r.cancelled = append(r.cancelled, t)
	return 2, nil
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
