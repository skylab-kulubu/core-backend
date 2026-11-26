package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authn"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/formsfeed"
)

type stubBackfills struct {
	queued bool
	err    error
	asked  authz.Principal
}

func (s *stubBackfills) Request(_ context.Context, p authz.Principal, _, _ uuid.UUID) (bool, error) {
	s.asked = p
	return s.queued, s.err
}

func TestFormBackfillAnswers(t *testing.T) {
	cases := []struct {
		queued     bool
		err        error
		status     int
		code       string
		retryAfter string
	}{
		{queued: true, status: 202},
		{queued: false, status: 200},
		{err: formsfeed.ErrBackfillEventNotFound, status: 404},
		{err: formsfeed.ErrBackfillForbidden, status: 403},
		{err: formsfeed.ErrBackfillCancelled, status: 409, code: "event_cancelled"},
		{err: formsfeed.ErrBackfillEnded, status: 409, code: "event_ended"},
		{err: formsfeed.ErrBackfillNotListed, status: 409, code: "form_not_listed"},
		{err: formsfeed.ErrBackfillFormInUse, status: 409, code: "form_in_use"},
		{err: formsfeed.ErrBackfillOff, status: 503, code: "forms_pull_off"},
		{err: formsfeed.ErrFormsAccessDenied, status: 403, code: "forms_access_denied"},
		{err: errors.Join(formsfeed.ErrFormsUnavailable, errors.New("formsfeed: Forms answered 503")), status: 503, code: "forms_unavailable"},
		{err: &formsfeed.BackfillCooldownError{RetryAfter: 89500 * time.Millisecond}, status: 429, code: "backfill_cooldown", retryAfter: "90"},
		{err: errors.New("database"), status: 500},
	}
	person := uuid.New()
	for _, tc := range cases {
		stub := &stubBackfills{queued: tc.queued, err: tc.err}
		app := fiber.New()
		app.Use(func(c fiber.Ctx) error {
			c.Locals(authn.LocalsIdentity, authn.Identity{ID: person, Groups: []string{"/UYELER/YK"}})
			return c.Next()
		})
		app.Post("/v1/events/:id/forms/:formId/backfill", NewFormBackfillHandler(stub, nil).Request)
		resp, err := app.Test(httptest.NewRequest("POST", "/v1/events/"+uuid.NewString()+"/forms/"+uuid.NewString()+"/backfill", nil))
		if err != nil {
			t.Fatal(err)
		}
		var body map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&body)
		resp.Body.Close()
		if resp.StatusCode != tc.status || (tc.code != "" && body["code"] != tc.code) {
			t.Fatalf("%v: %d %v", tc.err, resp.StatusCode, body)
		}
		if got := resp.Header.Get("Retry-After"); got != tc.retryAfter {
			t.Fatalf("Retry-After %q, want %q", got, tc.retryAfter)
		}
		if tc.status < 300 && body["queued"] != tc.queued {
			t.Fatalf("%d: body %v", tc.status, body)
		}
		if stub.asked.ID != person.String() {
			t.Fatalf("asked as %+v", stub.asked)
		}
	}
	app := fiber.New()
	app.Post("/v1/events/:id/forms/:formId/backfill", NewFormBackfillHandler(&stubBackfills{}, nil).Request)
	resp, _ := app.Test(httptest.NewRequest("POST", "/v1/events/x/forms/y/backfill", nil))
	if resp.StatusCode != 401 {
		t.Fatalf("no caller: %d", resp.StatusCode)
	}
}
