package httpx_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
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

func newGuestApplyEnv(t *testing.T) guestApplyEnv {
	t.Helper()
	keys := testauth.New(t)
	deps := memoryDeps()
	deps.ParseToken = keys.Parse()
	deps.GuestApplyMetrics = handlers.NewGuestApplyMetrics()
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
}

// apply posts a Guest apply. forwardedFor, when set, is the client address
// the edge proxy wrote: without it the request comes from inside the trusted
// proxy ranges, like the forms hop.
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
	answer := guestAnswer{status: resp.StatusCode, header: resp.Header}
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

// The callers found on 2026-10-03: the forms hop (no token, internal
// network, reads only the status code) and the Event hub (an operator's
// token, reads the Ticket). Anybody else gets nothing of an existing guest.
func TestGuestApplyAnswersEachCallerClass(t *testing.T) {
	t.Parallel()
	e := newGuestApplyEnv(t)
	leader := groupToken(t, e.keys, leaderSub, "/UYELER/ARGE/WEBLAB/LIDERLER")

	hub := e.apply(t, leader, publicClient, guest("Ada", "ada@example.com", "555"))
	if hub.status != fiber.StatusCreated || hub.body["guestPhoneNumber"] != "555" || hub.body["guestEmail"] != "ada@example.com" ||
		hub.body["id"] == nil || hub.body["ticketType"] != "GUEST" {
		t.Fatalf("Event hub: status %d body %v", hub.status, hub.body)
	}

	assertApplied(t, "forms hop", e.apply(t, "", "", guest("Ada", "ada@example.com", "")))
	assertApplied(t, "internet", e.apply(t, "", publicClient, guest("Mallory", "ADA@example.com", "999")))
	member := groupToken(t, e.keys, memberSub, "/UYELER/ARGE/GAMELAB")
	assertApplied(t, "person without Event rights", e.apply(t, member, publicClient, guest("Mallory", "ada@example.com", "")))
	assertApplied(t, "invalid token", e.apply(t, "not-a-jwt", publicClient, guest("Mallory", "ada@example.com", "")))

	forms := e.apply(t, serviceToken(t, e.keys, "forms"), "", guest("Augusta", "ada@example.com", ""))
	if forms.status != fiber.StatusCreated || forms.body["guestFirstName"] != "Augusta" || forms.body["guestPhoneNumber"] != "555" {
		t.Fatalf("forms service token: status %d body %v", forms.status, forms.body)
	}

	got := e.apply(t, leader, "", guest("Augusta", "ada@example.com", ""))
	if got.body["guestFirstName"] != "Augusta" || got.body["guestPhoneNumber"] != "555" {
		t.Fatalf("anonymous callers changed the guest: %v", got.body)
	}

	text := e.metrics(t)
	for _, line := range []string{
		`skylab_guest_apply_requests_total{caller="person",outcome="created"} 1`,
		`skylab_guest_apply_requests_total{caller="anonymous_internal",outcome="existing"} 1`,
		`skylab_guest_apply_requests_total{caller="anonymous_public",outcome="kept"} 2`,
		`skylab_guest_apply_requests_total{caller="person",outcome="kept"} 1`,
		`skylab_guest_apply_requests_total{caller="service",outcome="existing"} 1`,
		`skylab_guest_apply_requests_total{caller="person",outcome="existing"} 1`,
		"skylab_guest_apply_anonymous_public_total 2",
		"skylab_guest_apply_anonymous_internal_total 1",
		"skylab_guest_apply_person_total 3",
		"skylab_guest_apply_service_total 1",
	} {
		if !strings.Contains(text, line+"\n") {
			t.Fatalf("metrics lack %q:\n%s", line, text)
		}
	}
}

func TestGuestApplyLimitsEachInternetClient(t *testing.T) {
	t.Parallel()
	e := newGuestApplyEnv(t)
	limits := handlers.DefaultGuestApplyLimits()

	for i := range limits.PerClient {
		assertApplied(t, fmt.Sprint("request ", i), e.apply(t, "", publicClient, guest("Ada", fmt.Sprintf("ada%d@example.com", i), "")))
	}
	refused := e.apply(t, "", publicClient, guest("Ada", "one-more@example.com", ""))
	if refused.status != fiber.StatusTooManyRequests || refused.body["code"] != "guest_apply_rate_limited" ||
		!strings.HasPrefix(refused.header.Get(fiber.HeaderContentType), "application/problem+json") {
		t.Fatalf("over the budget: status %d header %v body %v", refused.status, refused.header, refused.body)
	}
	retry, err := strconv.Atoi(refused.header.Get(fiber.HeaderRetryAfter))
	if err != nil || retry <= 0 || retry > int(limits.PerClientWindow.Seconds()) {
		t.Fatalf("Retry-After %q", refused.header.Get(fiber.HeaderRetryAfter))
	}
	if refused.body["retryAfterSeconds"] != float64(retry) {
		t.Fatalf("retryAfterSeconds %v want %d", refused.body["retryAfterSeconds"], retry)
	}

	// Another client keeps its own budget; the forms hop and an operator
	// have none here.
	assertApplied(t, "another client", e.apply(t, "", "198.51.100.9", guest("Ada", "other@example.com", "")))
	leader := groupToken(t, e.keys, leaderSub, "/UYELER/ARGE/WEBLAB/LIDERLER")
	for i := range limits.PerClient + 5 {
		assertApplied(t, fmt.Sprint("forms hop ", i), e.apply(t, "", "", guest("Ada", fmt.Sprintf("form%d@example.com", i), "")))
		if got := e.apply(t, leader, publicClient, guest("Ada", fmt.Sprintf("hub%d@example.com", i), "")); got.status != fiber.StatusCreated {
			t.Fatalf("operator %d: status %d body %v", i, got.status, got.body)
		}
	}

	text := e.metrics(t)
	if !strings.Contains(text, `skylab_guest_apply_requests_total{caller="anonymous_public",outcome="rate_limited"} 1`+"\n") {
		t.Fatalf("rate limit not counted:\n%s", text)
	}
}

func TestGuestApplyLimitsEachGuestAcrossInternetClients(t *testing.T) {
	t.Parallel()
	e := newGuestApplyEnv(t)
	limits := handlers.DefaultGuestApplyLimits()

	for i := range limits.PerGuest {
		got := e.apply(t, "", fmt.Sprintf("203.0.113.%d", 10+i), guest("Ada", "ada@example.com", ""))
		assertApplied(t, fmt.Sprint("client ", i), got)
	}
	refused := e.apply(t, "", "203.0.113.99", guest("Ada", " ADA@example.com ", ""))
	if refused.status != fiber.StatusTooManyRequests || refused.body["code"] != "guest_apply_rate_limited" {
		t.Fatalf("over the guest budget: status %d body %v", refused.status, refused.body)
	}
	if got, want := refused.header.Get(fiber.HeaderRetryAfter), strconv.Itoa(int(limits.PerGuestWindow.Seconds())); got != want {
		t.Fatalf("Retry-After %q want %q", got, want)
	}
	// The other e-mail on the same client, and the forms hop for this one,
	// still go through.
	assertApplied(t, "other e-mail", e.apply(t, "", "203.0.113.99", guest("Ada", "grace@example.com", "")))
	assertApplied(t, "forms hop", e.apply(t, "", "", guest("Ada", "ada@example.com", "")))
}

// A valid token meets the account access gate as on every other route: a
// blocked account's token is refused, while the token-less forms hop is not
// asked about at all.
func TestGuestApplyRefusesABlockedAccountsToken(t *testing.T) {
	t.Parallel()
	keys := testauth.New(t)
	deps := memoryDeps()
	deps.ParseToken = keys.Parse()
	deps.GuestApplyMetrics = handlers.NewGuestApplyMetrics()
	gate := &httpAccessGate{decision: accessgate.Allowed}
	deps.AccountAccessGate = gate
	e := guestApplyEnv{app: httpx.New(deps), keys: keys}
	created := sendJSON(t, e.app, groupToken(t, keys, ykSub, "/UYELER/YK"), fiber.MethodPost, "/v1/events",
		`{"name":"Hack","location":"YTÜ","ownerTeam":"WEBLAB"}`)
	if created.status != fiber.StatusCreated {
		t.Fatalf("create event %d %v", created.status, created.body)
	}
	e.eventID = created.body["id"].(string)

	gate.decision = accessgate.Blocked
	leader := groupToken(t, keys, leaderSub, "/UYELER/ARGE/WEBLAB/LIDERLER")
	if got := e.apply(t, leader, "", guest("Ada", "ada@example.com", "")); got.status != fiber.StatusUnauthorized {
		t.Fatalf("blocked account: status %d body %v", got.status, got.body)
	}
	assertApplied(t, "forms hop", e.apply(t, "", "", guest("Ada", "ada@example.com", "")))
	if text := e.metrics(t); !strings.Contains(text, `skylab_guest_apply_requests_total{caller="person",outcome="refused"} 1`+"\n") {
		t.Fatalf("refusal not counted:\n%s", text)
	}
}
