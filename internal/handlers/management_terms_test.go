package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authn"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/identity"
	"github.com/skylab-kulubu/core-backend/internal/membershipterm"
	"github.com/skylab-kulubu/core-backend/internal/teamaudit"
)

type termsFake struct {
	filter   membershipterm.Filter
	endsAt   time.Time
	handover membershipterm.HandoverInput
	actor    teamaudit.Actor
	err      error
}

func (f *termsFake) List(_ context.Context, _ authz.Principal, filter membershipterm.Filter) ([]membershipterm.Item, error) {
	f.filter = filter
	return []membershipterm.Item{}, f.err
}

func (f *termsFake) Extend(ctx context.Context, _ authz.Principal, _ uuid.UUID, endsAt time.Time) (membershipterm.Item, error) {
	f.endsAt = endsAt
	f.actor, _ = teamaudit.ActorFrom(ctx)
	return membershipterm.Item{EndsAt: &endsAt}, f.err
}

func (f *termsFake) Handover(_ context.Context, _ authz.Principal, _ uuid.UUID, in membershipterm.HandoverInput) (membershipterm.Item, error) {
	f.handover = in
	return membershipterm.Item{}, f.err
}

func (f *termsFake) Remove(context.Context, authz.Principal, uuid.UUID) error { return f.err }

func termsApp(fake *termsFake) *fiber.App {
	h := NewManagementTermsHandler(fake)
	app := fiber.New(fiber.Config{ErrorHandler: ErrorHandler})
	app.Use(func(c fiber.Ctx) error {
		c.Locals(authn.LocalsIdentity, authn.Identity{ID: uuid.MustParse("11111111-1111-1111-1111-111111111111"), Groups: []string{"/UYELER/YK"}})
		return c.Next()
	})
	app.Get("/v1/management-memberships", h.List)
	app.Post("/v1/management-memberships/:termId/extend", h.Extend)
	app.Post("/v1/management-memberships/:termId/handover", h.Handover)
	app.Delete("/v1/management-memberships/:termId", h.Remove)
	return app
}

func termsCall(t *testing.T, app *fiber.App, method, target, body string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := app.Test(req, fiber.TestConfig{Timeout: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	out := map[string]any{}
	_ = json.Unmarshal(raw, &out)
	return resp.StatusCode, out
}

func TestManagementTermRoutes(t *testing.T) {
	t.Parallel()
	term := "/v1/management-memberships/" + uuid.NewString()
	fake := &termsFake{}
	app := termsApp(fake)

	season := uuid.NewString()
	if status, _ := termsCall(t, app, http.MethodGet, "/v1/management-memberships?season="+season+"&role=board&status=expired", ""); status != http.StatusOK ||
		fake.filter.SeasonID == nil || fake.filter.SeasonID.String() != season || fake.filter.Role != "board" || fake.filter.Status != membershipterm.StatusExpired {
		t.Fatalf("list %d %+v", status, fake.filter)
	}
	for _, bad := range []string{"?role=admin", "?status=gone", "?season=x"} {
		if status, _ := termsCall(t, app, http.MethodGet, "/v1/management-memberships"+bad, ""); status != http.StatusBadRequest {
			t.Errorf("%s: %d", bad, status)
		}
	}

	if status, _ := termsCall(t, app, http.MethodPost, term+"/extend", `{"endsAt":"2027-09-30"}`); status != http.StatusOK ||
		!fake.endsAt.Equal(time.Date(2027, 9, 30, 20, 59, 59, 0, time.UTC)) || fake.actor.ID.String() != "11111111-1111-1111-1111-111111111111" {
		t.Fatalf("extend %d %s %+v", status, fake.endsAt, fake.actor)
	}
	for _, body := range []string{`{"endsAt":"30.09.2027"}`, `{}`, `x`} {
		if status, _ := termsCall(t, app, http.MethodPost, term+"/extend", body); status != http.StatusBadRequest {
			t.Errorf("extend %s: %d", body, status)
		}
	}
	if status, _ := termsCall(t, app, http.MethodPost, "/v1/management-memberships/x/extend", `{"endsAt":"2027-09-30"}`); status != http.StatusBadRequest {
		t.Fatalf("a bad id: %d", status)
	}

	successor := uuid.New()
	if status, _ := termsCall(t, app, http.MethodPost, term+"/handover", `{"userId":"`+successor.String()+`","mode":"immediate","endsAt":"2028-05-31"}`); status != http.StatusOK ||
		fake.handover.UserID != successor || fake.handover.Mode != membershipterm.HandoverImmediate || fake.handover.EndsAt == nil {
		t.Fatalf("handover %d %+v", status, fake.handover)
	}
	if status, _ := termsCall(t, app, http.MethodPost, term+"/handover", `{"mode":"immediate"}`); status != http.StatusBadRequest {
		t.Fatalf("handover without a successor: %d", status)
	}

	for _, test := range []struct {
		err    error
		status int
		code   string
	}{
		{&membershipterm.RefusalError{Status: http.StatusForbidden, Code: membershipterm.CodeSelfExtension}, http.StatusForbidden, "self_extension"},
		{&membershipterm.RefusalError{Status: http.StatusUnprocessableEntity, Code: membershipterm.CodeTermTooLong}, http.StatusUnprocessableEntity, "term_too_long"},
		{&membershipterm.RefusalError{Status: http.StatusConflict, Code: membershipterm.CodeTermNotExpired}, http.StatusConflict, "term_not_expired"},
		{membershipterm.ErrOff, http.StatusNotFound, ""},
		{membershipterm.ErrNotFound, http.StatusNotFound, ""},
		{membershipterm.ErrInvalid, http.StatusBadRequest, ""},
		{identity.ErrForbidden, http.StatusForbidden, ""},
		{teamaudit.ErrUnavailable, http.StatusServiceUnavailable, "audit_unavailable"},
	} {
		fake.err = test.err
		status, body := termsCall(t, app, http.MethodDelete, term, "")
		if code, _ := body["code"].(string); status != test.status || code != test.code {
			t.Errorf("%v: %d %v", test.err, status, body)
		}
	}
}

type expiredFake struct {
	items []membershipterm.ExpiredMembership
}

func (f expiredFake) ExpiredFor(context.Context, uuid.UUID) ([]membershipterm.ExpiredMembership, error) {
	return f.items, nil
}

// The capabilities name the caller's expired management memberships: an
// empty list without the terms.
func TestCapabilitiesExpiredManagementMemberships(t *testing.T) {
	t.Parallel()
	ident := authn.Identity{ID: uuid.New(), Groups: []string{"/UYELER/YK"}}
	off := getCapabilities(t, capabilitiesApp(t, ident, nil, &bytes.Buffer{}))
	if string(off["expiredManagementMemberships"]) != "[]" {
		t.Fatalf("off %s", off["expiredManagementMemberships"])
	}
	group := uuid.New()
	h := NewCapabilitiesHandler(authz.NewAuthorizer(authz.DefaultPolicy()), nil).
		WithExpiredMemberships(expiredFake{items: []membershipterm.ExpiredMembership{{TermID: uuid.New(), GroupID: group, GroupPath: "/UYELER/YK", Role: "board"}}})
	app := fiber.New(fiber.Config{ErrorHandler: ErrorHandler})
	app.Use(func(c fiber.Ctx) error {
		c.Locals(authn.LocalsIdentity, ident)
		return c.Next()
	})
	app.Get("/v1/users/me/capabilities", h.Get)
	var got []membershipterm.ExpiredMembership
	if err := json.Unmarshal(getCapabilities(t, app)["expiredManagementMemberships"], &got); err != nil || len(got) != 1 || got[0].GroupID != group || got[0].Role != "board" {
		t.Fatalf("expired %+v %v", got, err)
	}
}
