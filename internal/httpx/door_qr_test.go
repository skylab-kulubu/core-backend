package httpx_test

import (
	"encoding/json"
	"io"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/doorqr"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/handlers"
	"github.com/skylab-kulubu/core-backend/internal/httpx"
	"github.com/skylab-kulubu/core-backend/internal/testauth"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
)

type doorQREnv struct {
	app       *fiber.App
	keys      *testauth.Bundle
	yk        string
	sessionID string
}

func newDoorQREnv(t *testing.T, limits *handlers.DoorQRLimits) doorQREnv {
	t.Helper()
	keys := testauth.New(t)
	az := authz.NewAuthorizer(authz.DefaultPolicy())
	events := event.NewMemoryStore()
	gate := doorqr.NewGate([]byte("assembled door qr key, assembled"), doorqr.Config{Mode: doorqr.ModeQR})
	deps := memoryDeps()
	deps.ParseToken = keys.Parse()
	deps.Events = event.NewService(events, az)
	deps.Tickets = ticket.NewService(ticket.NewMemoryStore(), events, az, gate)
	deps.GuestCheckInMetrics = gate
	deps.DoorQRLimits = limits
	app := httpx.New(deps)

	yk := groupToken(t, keys, ykSub, "/UYELER/YK")
	created := sendJSON(t, app, yk, fiber.MethodPost, "/v1/events", `{"name":"Hack","location":"YTÜ","ownerTeam":"WEBLAB"}`)
	if created.status != fiber.StatusCreated {
		t.Fatalf("event %d %v", created.status, created.body)
	}
	eventID := created.body["id"].(string)
	day := sendJSON(t, app, yk, fiber.MethodPost, "/v1/event-days", `{"eventId":"`+eventID+`","name":"Day 1"}`)
	if day.status != fiber.StatusCreated {
		t.Fatalf("day %d %v", day.status, day.body)
	}
	sess := sendJSON(t, app, yk, fiber.MethodPost, "/v1/sessions",
		`{"eventDayId":"`+day.body["id"].(string)+`","title":"Opening","speakerName":"Ada","sessionType":"PRESENTATION"}`)
	if sess.status != fiber.StatusCreated {
		t.Fatalf("session %d %v", sess.status, sess.body)
	}
	applied := sendJSON(t, app, yk, fiber.MethodPost, "/v1/events/"+eventID+"/applications/guest",
		`{"firstName":"Ada","lastName":"Lovelace","email":"ada@example.com"}`)
	if applied.status != fiber.StatusCreated && applied.status != fiber.StatusOK {
		t.Fatalf("guest apply %d %v", applied.status, applied.body)
	}
	return doorQREnv{app: app, keys: keys, yk: yk, sessionID: sess.body["id"].(string)}
}

func (e doorQREnv) mint(t *testing.T, token string) jsonResponse {
	t.Helper()
	return sendJSON(t, e.app, token, fiber.MethodPost, "/v1/sessions/"+e.sessionID+"/door-qr", "")
}

// guest posts a guest check-in from client (the address the edge proxy
// wrote; empty: from inside the trusted ranges) and returns status and code.
func (e doorQREnv) guest(t *testing.T, client, body string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(fiber.MethodPost, "/v1/sessions/"+e.sessionID+"/check-in/guest", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if client != "" {
		req.Header.Set("X-Forwarded-For", client)
	}
	resp, err := e.app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	var problem struct {
		Code string `json:"code"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&problem)
	return resp.StatusCode, problem.Code
}

// TestGuestSelfCheckInWithDoorQR runs the whole door QR flow through the
// assembled app in qr mode: the door screen mints with its token, the guest
// checks in without one.
func TestGuestSelfCheckInWithDoorQR(t *testing.T) {
	t.Parallel()
	e := newDoorQREnv(t, nil)
	app, keys, sessionID := e.app, e.keys, e.sessionID
	yk := e.yk

	member := groupToken(t, keys, memberSub, "/UYELER/ARGE/GAMELAB")
	if got := sendJSON(t, app, member, fiber.MethodPost, "/v1/sessions/"+sessionID+"/door-qr", ""); got.status != fiber.StatusForbidden {
		t.Fatalf("member mint %d", got.status)
	}
	minted := sendJSON(t, app, yk, fiber.MethodPost, "/v1/sessions/"+sessionID+"/door-qr", "")
	if minted.status != fiber.StatusCreated {
		t.Fatalf("mint %d %v", minted.status, minted.body)
	}
	token, _ := minted.body["token"].(string)

	guest := func(body string) int {
		t.Helper()
		status, _ := e.guest(t, "", body)
		return status
	}
	if status := guest(`{"email":"ada@example.com"}`); status != fiber.StatusForbidden {
		t.Fatalf("without door qr %d", status)
	}
	if status := guest(`{"email":"ada@example.com","doorToken":"` + token + `"}`); status != fiber.StatusCreated {
		t.Fatalf("with door qr %d", status)
	}

	resp, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/v1/metrics", nil))
	if err != nil {
		t.Fatal(err)
	}
	text, _ := io.ReadAll(resp.Body)
	for _, want := range []string{
		`skylab_guest_self_checkin_total{door_qr="absent",outcome="door_qr_required"} 1`,
		`skylab_guest_self_checkin_total{door_qr="present",outcome="checked_in"} 1`,
		`skylab_guest_self_checkin_mode{mode="qr"} 1`,
	} {
		if !strings.Contains(string(text), want) {
			t.Errorf("metrics miss %s", want)
		}
	}
}

// TestGuestCheckInFailuresAreBudgetedPerEmail: one guest who keeps failing
// from an address spends their own budget (10 a minute by default), not that
// of everyone else behind the same address, a hall's Wi-Fi.
func TestGuestCheckInFailuresAreBudgetedPerEmail(t *testing.T) {
	t.Parallel()
	e := newDoorQREnv(t, nil)
	minted := e.mint(t, e.yk)
	if minted.status != fiber.StatusCreated {
		t.Fatalf("mint %d", minted.status)
	}
	token := minted.body["token"].(string)
	const client = "203.0.113.7"
	junk := `{"email":"nobody@example.com","doorToken":"` + token + `"}`
	for i := range 9 {
		if status, _ := e.guest(t, client, junk); status != fiber.StatusNotFound {
			t.Fatalf("failure %d: %d", i+1, status)
		}
	}
	// A success does not count against the address.
	if status, _ := e.guest(t, client, `{"email":"ada@example.com","doorToken":"`+token+`"}`); status != fiber.StatusCreated {
		t.Fatalf("guest %d", status)
	}
	// Case and spaces aside it is the same e-mail.
	if status, _ := e.guest(t, client, `{"email":" Nobody@Example.COM ","doorToken":"`+token+`"}`); status != fiber.StatusNotFound {
		t.Fatalf("tenth failure %d", status)
	}
	if status, code := e.guest(t, client, junk); status != fiber.StatusTooManyRequests || code != "guest_check_in_rate_limited" {
		t.Fatalf("eleventh %d %q", status, code)
	}
	// Everyone else behind the address is still served: another e-mail, and
	// a guest tapping again after checking in (409 counts as a failure).
	if status, _ := e.guest(t, client, `{"email":"someone@example.com","doorToken":"`+token+`"}`); status != fiber.StatusNotFound {
		t.Fatalf("other e-mail behind the address %d", status)
	}
	if status, _ := e.guest(t, client, `{"email":"ada@example.com","doorToken":"`+token+`"}`); status != fiber.StatusConflict {
		t.Fatalf("again %d", status)
	}
	// Another address has its own budget, and the token was not used up by
	// the junk.
	if status, _ := e.guest(t, "203.0.113.8", junk); status != fiber.StatusNotFound {
		t.Fatalf("other client %d", status)
	}
}

// TestGuestCheckInFailuresAreBudgetedPerClient: trying many e-mails from one
// address stays bounded by the address's budget.
func TestGuestCheckInFailuresAreBudgetedPerClient(t *testing.T) {
	t.Parallel()
	limits := handlers.DefaultDoorQRLimits()
	limits.GuestFailuresPerClient = 3
	e := newDoorQREnv(t, &limits)
	minted := e.mint(t, e.yk)
	if minted.status != fiber.StatusCreated {
		t.Fatalf("mint %d", minted.status)
	}
	token := minted.body["token"].(string)
	const client = "203.0.113.9"
	junk := func(i int) string {
		return `{"email":"nobody` + strconv.Itoa(i) + `@example.com","doorToken":"` + token + `"}`
	}
	for i := range 3 {
		if status, _ := e.guest(t, client, junk(i)); status != fiber.StatusNotFound {
			t.Fatalf("failure %d: %d", i+1, status)
		}
	}
	if status, code := e.guest(t, client, junk(3)); status != fiber.StatusTooManyRequests || code != "guest_check_in_rate_limited" {
		t.Fatalf("fourth e-mail %d %q", status, code)
	}
	if status, _ := e.guest(t, "203.0.113.10", junk(3)); status != fiber.StatusNotFound {
		t.Fatalf("other client %d", status)
	}
}

// TestGuestCheckInHallBehindOneAddress: with the default budgets a hall's
// worth of failures behind one address (mistyped e-mails, second taps) is
// not refused.
func TestGuestCheckInHallBehindOneAddress(t *testing.T) {
	t.Parallel()
	e := newDoorQREnv(t, nil)
	minted := e.mint(t, e.yk)
	if minted.status != fiber.StatusCreated {
		t.Fatalf("mint %d", minted.status)
	}
	token := minted.body["token"].(string)
	const client = "203.0.113.11"
	for i := range 120 {
		body := `{"email":"guest` + strconv.Itoa(i) + `@example.com","doorToken":"` + token + `"}`
		if status, code := e.guest(t, client, body); status != fiber.StatusNotFound {
			t.Fatalf("guest %d: %d %q", i+1, status, code)
		}
	}
	if status, _ := e.guest(t, client, `{"email":"ada@example.com","doorToken":"`+token+`"}`); status != fiber.StatusCreated {
		t.Fatalf("ada %d", status)
	}
}

func TestDoorQRMintIsBudgetedPerPerson(t *testing.T) {
	t.Parallel()
	limits := handlers.DefaultDoorQRLimits()
	limits.MintsPerSubject = 2
	e := newDoorQREnv(t, &limits)
	for i := range 2 {
		if got := e.mint(t, e.yk); got.status != fiber.StatusCreated {
			t.Fatalf("mint %d: %d", i+1, got.status)
		}
	}
	got := e.mint(t, e.yk)
	if got.status != fiber.StatusTooManyRequests || got.body["code"] != "door_qr_rate_limited" {
		t.Fatalf("third mint %d %v", got.status, got.body)
	}
	other := groupToken(t, e.keys, leaderSub, "/UYELER/ARGE/WEBLAB/LIDERLER")
	if got := e.mint(t, other); got.status != fiber.StatusCreated {
		t.Fatalf("another person %d", got.status)
	}
}
