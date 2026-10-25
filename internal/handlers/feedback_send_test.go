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

// sender answers SendNow as told, recording the targets.
type sender struct {
	targets []feedback.Target
	err     error
}

func (s *sender) SendNow(_ context.Context, t feedback.Target, _ time.Time) (bool, int, error) {
	if s.err != nil {
		return false, 0, s.err
	}
	for _, seen := range s.targets {
		if seen.EventID == t.EventID && (seen.SessionID == nil) == (t.SessionID == nil) {
			return false, 0, nil
		}
	}
	s.targets = append(s.targets, t)
	return true, 3, nil
}

// "Şimdi gönder" (ticket 06): who may edit the Event; once; 409 with a code
// when there is no form or it may not go.
func TestFeedbackSendNowEndpoints(t *testing.T) {
	t.Parallel()
	e := newDraftEnv(t)
	send := &sender{}
	app := func(ident authn.Identity) *fiber.App {
		evSvc := event.NewService(e.events, authz.NewAuthorizer(authz.DefaultPolicy()))
		h := NewFeedbackHandler(evSvc, authz.NewAuthorizer(authz.DefaultPolicy()), fixedCounts{}).SendWith(send)
		a := fiber.New()
		a.Use(func(c fiber.Ctx) error {
			if ident.ID != uuid.Nil || len(ident.Groups) > 0 {
				c.Locals(authn.LocalsIdentity, ident)
			}
			return c.Next()
		})
		a.Post("/v1/events/:id/feedback/send", h.SendEventNow)
		a.Post("/v1/sessions/:id/feedback/send", h.SendSessionNow)
		return a
	}
	pub := "/v1/events/" + e.published.ID.String() + "/feedback/send"
	for _, tc := range []struct {
		name   string
		ident  authn.Identity
		status int
	}{
		{"signed out", draftAnon, fiber.StatusUnauthorized},
		{"member", draftMember, fiber.StatusForbidden},
		{"DK", draftDK, fiber.StatusForbidden},
		{"editor", draftEditor, fiber.StatusAccepted},
		{"editor again", draftEditor, fiber.StatusOK},
	} {
		if got := draftCall(t, app(tc.ident), fiber.MethodPost, pub, ""); got.status != tc.status {
			t.Errorf("%s: %d %s", tc.name, got.status, got.raw)
		}
	}
	got := draftCall(t, app(draftEditor), fiber.MethodPost, "/v1/sessions/"+e.pubSession.ID.String()+"/feedback/send", "")
	if got.status != fiber.StatusAccepted || len(send.targets) != 2 || send.targets[1].SessionID == nil || *send.targets[1].SessionID != e.pubSession.ID {
		t.Fatalf("session %d %s %+v", got.status, got.raw, send.targets)
	}
	if got := draftCall(t, app(draftOtherLeader), fiber.MethodPost, "/v1/events/"+e.draft.ID.String()+"/feedback/send", ""); got.status != fiber.StatusNotFound {
		t.Fatalf("another team's draft %d", got.status)
	}
	for err, code := range map[error]string{feedback.ErrNoForm: "feedback_form_missing", feedback.ErrNotSendable: "feedback_not_sendable"} {
		send.err = err
		if got := draftCall(t, app(draftEditor), fiber.MethodPost, pub, ""); got.status != fiber.StatusConflict || got.body["code"] != code {
			t.Errorf("%v: %d %s", err, got.status, got.raw)
		}
	}
}
