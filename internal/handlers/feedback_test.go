package handlers

import (
	"context"
	"log"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/requestid"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authn"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/feedback"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

type fixedCounts struct{ counts feedback.Counts }

func (f fixedCounts) Counts(context.Context, uuid.UUID) (feedback.Counts, error) {
	return f.counts, nil
}

// GET /v1/events/{id}/feedback: the Event's editors and DK read the counts;
// a member does not; a draft is not there for who may not see it.
func TestFeedbackCountsEndpoint(t *testing.T) {
	t.Parallel()
	e := newDraftEnv(t)
	session := uuid.New()
	counter := fixedCounts{feedback.Counts{Event: feedback.Count{Sent: 12, Answered: 5}, Sessions: []feedback.Count{{SessionID: &session, Sent: 4, Answered: 1}}}}
	app := func(ident authn.Identity) *fiber.App {
		evSvc := event.NewService(e.events, authz.NewAuthorizer(authz.DefaultPolicy()))
		h := NewFeedbackHandler(evSvc, authz.NewAuthorizer(authz.DefaultPolicy()), counter)
		a := fiber.New()
		a.Use(func(c fiber.Ctx) error {
			if ident.ID != uuid.Nil || len(ident.Groups) > 0 {
				c.Locals(authn.LocalsIdentity, ident)
			}
			return c.Next()
		})
		a.Get("/v1/events/:id/feedback", h.Counts)
		return a
	}
	pub, draft := e.published.ID.String(), e.draft.ID.String()
	for _, tc := range []struct {
		name   string
		ident  authn.Identity
		path   string
		status int
	}{
		{"signed out", draftAnon, pub, fiber.StatusUnauthorized},
		{"member", draftMember, pub, fiber.StatusForbidden},
		{"another team's leader", draftOtherLeader, pub, fiber.StatusForbidden},
		{"another team's leader, draft", draftOtherLeader, draft, fiber.StatusNotFound},
		{"DK", draftDK, pub, fiber.StatusOK},
		{"editor", draftEditor, draft, fiber.StatusOK},
	} {
		got := draftCall(t, app(tc.ident), fiber.MethodGet, "/v1/events/"+tc.path+"/feedback", "")
		if got.status != tc.status {
			t.Errorf("%s: %d %s", tc.name, got.status, got.raw)
			continue
		}
		if got.status == fiber.StatusOK {
			ev, _ := got.body["event"].(map[string]any)
			sessions, _ := got.body["sessions"].([]any)
			if ev["sent"] != float64(12) || ev["answered"] != float64(5) || len(sessions) != 1 {
				t.Errorf("%s: %s", tc.name, got.raw)
			}
		}
	}
}

type invitesSeen struct{ codes []string }

func (s *invitesSeen) MarkAnswered(_ context.Context, _ uuid.UUID, code string, _ *uuid.UUID) (bool, error) {
	s.codes = append(s.codes, code)
	return true, nil
}

// The report's invite reaches the marking, and the log line does not carry
// it.
func TestFormResponseInviteIsPassedOnNotLogged(t *testing.T) {
	t.Parallel()
	events := event.NewMemoryStore()
	form := uuid.New()
	if _, err := events.Create(t.Context(), event.Event{Name: "Hack", Location: "YTÜ", OwnerTeam: "WEBLAB", FeedbackFormURL: "https://forms.yildizskylab.com/" + form.String()}); err != nil {
		t.Fatal(err)
	}
	seen := &invitesSeen{}
	var logs lockedBuffer
	h := NewTicketHandler(ticket.NewService(ticket.NewMemoryStore(), events, authz.NewAuthorizer(authz.DefaultPolicy()), user.NewMemoryStore(), seen))
	h.logger = log.New(&logs, "", 0)
	app := fiber.New()
	app.Use(requestid.New())
	app.Use(func(c fiber.Ctx) error {
		c.Locals(authn.LocalsIdentity, authn.Identity{ID: uuid.New(), Client: "forms", ServiceAccount: true, Product: authz.ProductForms, Roles: []string{"ticket:forms"}})
		return c.Next()
	})
	app.Post("/v1/forms/:formId/responses", h.RecordFormResponse)
	code := "Q2FuIHlvdSBzZWUgbWU_Pw"
	req := httptest.NewRequest(fiber.MethodPost, "/v1/forms/"+form.String()+"/responses",
		strings.NewReader(`{"responseId":"`+uuid.NewString()+`","status":"accepted","invite":"`+code+`"}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req)
	if err != nil || resp.StatusCode != fiber.StatusNoContent {
		t.Fatalf("%v %v", resp, err)
	}
	if len(seen.codes) != 1 || seen.codes[0] != code {
		t.Fatalf("codes %v", seen.codes)
	}
	if strings.Contains(logs.String(), code) {
		t.Fatalf("log carries the code: %s", logs.String())
	}
}
