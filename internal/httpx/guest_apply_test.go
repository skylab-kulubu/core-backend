package httpx_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/golang-jwt/jwt/v5"
	"github.com/skylab-kulubu/core-backend/internal/accessgate"
	"github.com/skylab-kulubu/core-backend/internal/handlers"
	"github.com/skylab-kulubu/core-backend/internal/httpx"
	"github.com/skylab-kulubu/core-backend/internal/testauth"
)

// guestApplyEnv is the assembled app with one WEBLAB Event, as production
// wires it: optional bearer, access gate, limits, counters.
type guestApplyEnv struct {
	app     *fiber.App
	keys    *testauth.Bundle
	eventID string
}

const (
	ykSub     = "11111111-1111-1111-1111-111111111111"
	leaderSub = "22222222-2222-2222-2222-222222222222"
	memberSub = "33333333-3333-3333-3333-333333333333"
)

func newGuestApplyEnv(t *testing.T, configure ...func(*httpx.Deps)) guestApplyEnv {
	t.Helper()
	keys := testauth.New(t)
	deps := memoryDeps()
	deps.ParseToken = keys.Parse()
	deps.GuestApplyMetrics = handlers.NewGuestApplyMetrics()
	for _, change := range configure {
		change(&deps)
	}
	app := httpx.New(deps)
	created := sendJSON(t, app, groupToken(t, keys, ykSub, "/UYELER/YK"), fiber.MethodPost, "/v1/events",
		`{"name":"Hack","location":"YTÜ","ownerTeam":"WEBLAB"}`)
	if created.status != fiber.StatusCreated {
		t.Fatalf("create event %d %v", created.status, created.body)
	}
	return guestApplyEnv{app: app, keys: keys, eventID: created.body["id"].(string)}
}

func groupToken(t *testing.T, keys *testauth.Bundle, sub string, groups ...string) string {
	t.Helper()
	return keys.Token(t, jwt.MapClaims{"sub": sub, "email": sub + "@example.com", "groups": groups})
}

type guestAnswer struct {
	status int
	header http.Header
	body   map[string]any
	raw    string
}

// apply posts a Guest apply. forwardedFor, when set, is the client address
// the edge proxy wrote, so the request came through the public ingress:
// without it the request comes from inside the trusted proxy ranges, like
// the old forms hop.
func (e guestApplyEnv) apply(t *testing.T, token, forwardedFor, body string) guestAnswer {
	t.Helper()
	req := httptest.NewRequest(fiber.MethodPost, "/v1/events/"+e.eventID+"/applications/guest", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if forwardedFor != "" {
		req.Header.Set("X-Forwarded-For", forwardedFor)
	}
	resp, err := e.app.Test(req, fiber.TestConfig{Timeout: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	answer := guestAnswer{status: resp.StatusCode, header: resp.Header, raw: string(raw)}
	if resp.StatusCode == fiber.StatusNotFound && strings.HasPrefix(resp.Header.Get(fiber.HeaderContentType), "text/plain") {
		return answer
	}
	if err := json.Unmarshal(raw, &answer.body); err != nil {
		t.Fatalf("status %d body %s: %v", resp.StatusCode, raw, err)
	}
	return answer
}

func (e guestApplyEnv) metrics(t *testing.T) string {
	t.Helper()
	resp, err := e.app.Test(httptest.NewRequest(fiber.MethodGet, "/v1/metrics", nil))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("metrics %d %s", resp.StatusCode, raw)
	}
	return string(raw)
}

func guest(first, email, phone string) string {
	body := map[string]string{"firstName": first, "lastName": "Lovelace", "email": email}
	if phone != "" {
		body["phoneNumber"] = phone
	}
	raw, _ := json.Marshal(body)
	return string(raw)
}

const publicClient = "203.0.113.7"

var personalFields = []string{"id", "guestFirstName", "guestLastName", "guestEmail", "guestPhoneNumber", "checkIns", "eventId", "event", "ticketType"}

func assertApplied(t *testing.T, who string, got guestAnswer) {
	t.Helper()
	if got.status != fiber.StatusCreated || got.body["status"] != "applied" || len(got.body) != 1 {
		t.Fatalf("%s: status %d body %v", who, got.status, got.body)
	}
	for _, field := range personalFields {
		if _, ok := got.body[field]; ok {
			t.Fatalf("%s: answer carries %s: %v", who, field, got.body)
		}
	}
}

// assertHidden is the edge's answer to a token-less request: a bare 404,
// as for a route that is not there.
func assertHidden(t *testing.T, who string, got guestAnswer) {
	t.Helper()
	if got.status != fiber.StatusNotFound || got.raw != "Not Found" || got.header.Get(fiber.HeaderCacheControl) != "no-store" {
		t.Fatalf("%s: status %d header %v body %q", who, got.status, got.header, got.raw)
	}
}

func assertForbidden(t *testing.T, who string, got guestAnswer) {
	t.Helper()
	if got.status != fiber.StatusForbidden {
		t.Fatalf("%s: status %d body %v", who, got.status, got.body)
	}
}

// The callers read on 2026-11-24: the Event hub (an operator's token
// through the edge, reads the Ticket) and the forms service with
// ticket:forms on the internal network (it no longer calls the route; it
// reports its answers instead, docs/form-response-tickets.md). The internet
// without a token does not find the route; a token holder who is neither is
// refused before anything is written.
func TestGuestApplyAnswersEachCallerClass(t *testing.T) {
	t.Parallel()
	e := newGuestApplyEnv(t)
	leader := groupToken(t, e.keys, leaderSub, "/UYELER/ARGE/WEBLAB/LIDERLER")

	hub := e.apply(t, leader, publicClient, guest("Ada", "ada@example.com", "555"))
	if hub.status != fiber.StatusCreated || hub.body["guestPhoneNumber"] != "555" || hub.body["guestEmail"] != "ada@example.com" ||
		hub.body["id"] == nil || hub.body["ticketType"] != "GUEST" {
		t.Fatalf("Event hub: status %d body %v", hub.status, hub.body)
	}

	assertHidden(t, "internet", e.apply(t, "", publicClient, guest("Mallory", "ADA@example.com", "999")))
	assertHidden(t, "internet, new e-mail", e.apply(t, "", publicClient, guest("Mallory", "mallory@example.com", "")))
	assertHidden(t, "invalid token through the edge", e.apply(t, "not-a-jwt", publicClient, guest("Mallory", "ada@example.com", "")))
	assertHidden(t, "private visitor through the edge", e.apply(t, "", "10.0.1.7", guest("Mallory", "mallory@example.com", "")))
	member := groupToken(t, e.keys, memberSub, "/UYELER/ARGE/GAMELAB")
	assertForbidden(t, "person without Event rights", e.apply(t, member, publicClient, guest("Mallory", "ada@example.com", "")))
	assertForbidden(t, "person without Event rights, new e-mail", e.apply(t, member, publicClient, guest("Mallory", "mallory@example.com", "")))
	assertForbidden(t, "forms without ticket:forms", e.apply(t, serviceToken(t, e.keys, "forms"), "", guest("Mallory", "mallory@example.com", "")))
	assertForbidden(t, "service account of no product", e.apply(t, serviceToken(t, e.keys, "some-other-client", "ticket:forms"), "", guest("Mallory", "mallory@example.com", "")))
	assertApplied(t, "internal hop", e.apply(t, "", "", guest("Ada", "ada@example.com", "")))

	forms := e.apply(t, serviceToken(t, e.keys, "forms", "ticket:forms"), "", guest("Augusta", "ada@example.com", ""))
	if forms.status != fiber.StatusCreated || forms.body["guestFirstName"] != "Augusta" || forms.body["guestPhoneNumber"] != "555" {
		t.Fatalf("forms service token: status %d body %v", forms.status, forms.body)
	}
	created := e.apply(t, serviceToken(t, e.keys, "forms", "ticket:forms"), "", guest("Grace", "grace@example.com", ""))
	if created.status != fiber.StatusCreated || created.body["guestEmail"] != "grace@example.com" || created.body["id"] == nil {
		t.Fatalf("forms service token, new e-mail: status %d body %v", created.status, created.body)
	}

	got := e.apply(t, leader, publicClient, guest("Augusta", "ada@example.com", ""))
	if got.status != fiber.StatusCreated || got.body["guestFirstName"] != "Augusta" || got.body["guestPhoneNumber"] != "555" {
		t.Fatalf("the refused callers changed the guest: %v", got.body)
	}
	// Nobody refused wrote mallory's Ticket: the operator's is a new one
	// (counted created below).
	if got := e.apply(t, leader, publicClient, guest("Mallory", "mallory@example.com", "")); got.status != fiber.StatusCreated || got.body["guestFirstName"] != "Mallory" {
		t.Fatalf("mallory: status %d body %v", got.status, got.body)
	}

	text := e.metrics(t)
	for _, line := range []string{
		`skylab_guest_apply_requests_total{caller="person",outcome="created"} 2`,
		`skylab_guest_apply_requests_total{caller="anonymous_public",outcome="not_found"} 4`,
		`skylab_guest_apply_requests_total{caller="person",outcome="refused"} 2`,
		`skylab_guest_apply_requests_total{caller="service",outcome="refused"} 2`,
		`skylab_guest_apply_requests_total{caller="anonymous_internal",outcome="existing"} 1`,
		`skylab_guest_apply_requests_total{caller="service",outcome="existing"} 1`,
		`skylab_guest_apply_requests_total{caller="service",outcome="created"} 1`,
		`skylab_guest_apply_requests_total{caller="person",outcome="existing"} 1`,
		"skylab_guest_apply_anonymous_public_total 4",
		"skylab_guest_apply_anonymous_internal_total 1",
		"skylab_guest_apply_person_total 5",
		"skylab_guest_apply_service_total 4",
	} {
		if !strings.Contains(text, line+"\n") {
			t.Fatalf("metrics lack %q:\n%s", line, text)
		}
	}
}

// A valid token is not an unlimited licence: an operator's or the forms
// service's token must not write guest Tickets without end. The budget is
// wide enough for an operator adding guests by hand.
func TestGuestApplyLimitsEachTokenSubject(t *testing.T) {
	t.Parallel()
	e := newGuestApplyEnv(t)
	limits := handlers.DefaultGuestApplyLimits()
	leader := groupToken(t, e.keys, leaderSub, "/UYELER/ARGE/WEBLAB/LIDERLER")
	forms := serviceToken(t, e.keys, "forms", "ticket:forms")

	for i := range limits.PerSubject {
		if got := e.apply(t, leader, publicClient, guest("Ada", fmt.Sprintf("m%d@example.com", i), "")); got.status != fiber.StatusCreated {
			t.Fatalf("leader %d: status %d body %v", i, got.status, got.body)
		}
		if got := e.apply(t, forms, "", guest("Ada", fmt.Sprintf("f%d@example.com", i), "")); got.status != fiber.StatusCreated {
			t.Fatalf("forms %d: status %d body %v", i, got.status, got.body)
		}
	}
	for who, token := range map[string]string{"leader": leader, "forms": forms} {
		refused := e.apply(t, token, "", guest("Ada", "one-more-"+who+"@example.com", ""))
		if refused.status != fiber.StatusTooManyRequests || refused.body["code"] != "guest_apply_rate_limited" ||
			refused.header.Get(fiber.HeaderRetryAfter) == "" {
			t.Fatalf("%s over the budget: status %d header %v body %v", who, refused.status, refused.header, refused.body)
		}
	}
	yk := groupToken(t, e.keys, ykSub, "/UYELER/YK")
	if got := e.apply(t, yk, publicClient, guest("Ada", "yk@example.com", "")); got.status != fiber.StatusCreated {
		t.Fatalf("another subject: status %d body %v", got.status, got.body)
	}
	assertApplied(t, "internal hop without a token", e.apply(t, "", "", guest("Ada", "hop@example.com", "")))

	text := e.metrics(t)
	for _, line := range []string{
		`skylab_guest_apply_requests_total{caller="person",outcome="rate_limited"} 1`,
		`skylab_guest_apply_requests_total{caller="service",outcome="rate_limited"} 1`,
	} {
		if !strings.Contains(text, line+"\n") {
			t.Fatalf("metrics lack %q:\n%s", line, text)
		}
	}
}

// A valid token meets the account access gate as on every other route: a
// blocked account's token is refused, while the token-less internal hop is
// not asked about at all.
func TestGuestApplyRefusesABlockedAccountsToken(t *testing.T) {
	t.Parallel()
	gate := &httpAccessGate{decision: accessgate.Allowed}
	e := newGuestApplyEnv(t, func(deps *httpx.Deps) { deps.AccountAccessGate = gate })
	keys := e.keys

	gate.decision = accessgate.Blocked
	leader := groupToken(t, keys, leaderSub, "/UYELER/ARGE/WEBLAB/LIDERLER")
	if got := e.apply(t, leader, "", guest("Ada", "ada@example.com", "")); got.status != fiber.StatusUnauthorized {
		t.Fatalf("blocked account: status %d body %v", got.status, got.body)
	}
	assertApplied(t, "internal hop", e.apply(t, "", "", guest("Ada", "ada@example.com", "")))
	if text := e.metrics(t); !strings.Contains(text, `skylab_guest_apply_requests_total{caller="person",outcome="refused"} 1`+"\n") {
		t.Fatalf("refusal not counted:\n%s", text)
	}
}
