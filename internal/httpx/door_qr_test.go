package httpx_test

import (
	"encoding/json"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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
	dayID     string
}

func newDoorQREnv(t *testing.T, limits *handlers.DoorQRLimits) doorQREnv {
	t.Helper()
	return newDoorQREnvWith(t, limits, doorqr.Config{Mode: doorqr.ModeQR})
}

func newDoorQREnvWith(t *testing.T, limits *handlers.DoorQRLimits, cfg doorqr.Config) doorQREnv {
	t.Helper()
	keys := testauth.New(t)
	az := authz.NewAuthorizer(authz.DefaultPolicy())
	events := event.NewMemoryStore()
	gate := doorqr.NewGate([]byte("assembled door qr key, assembled"), cfg)
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
	return doorQREnv{app: app, keys: keys, yk: yk, sessionID: sess.body["id"].(string), dayID: day.body["id"].(string)}
}

// timedSession adds a Session from start to end to the env's day.
func (e doorQREnv) timedSession(t *testing.T, start, end time.Time) string {
	t.Helper()
	sess := sendJSON(t, e.app, e.yk, fiber.MethodPost, "/v1/sessions",
		`{"eventDayId":"`+e.dayID+`","title":"Talk","speakerName":"Ada","sessionType":"PRESENTATION","startTime":"`+
			start.UTC().Format(time.RFC3339)+`","endTime":"`+end.UTC().Format(time.RFC3339)+`"}`)
	if sess.status != fiber.StatusCreated {
		t.Fatalf("session %d %v", sess.status, sess.body)
	}
	return sess.body["id"].(string)
}

func (e doorQREnv) mintStatic(t *testing.T, token, sessionID string) jsonResponse {
	t.Helper()
	return sendJSON(t, e.app, token, fiber.MethodPost, "/v1/sessions/"+sessionID+"/door-qr?use=static", "")
}

// guestAt posts a guest check-in to sessionID and returns status and code.
func (e doorQREnv) guestAt(t *testing.T, sessionID, body string) (int, string) {
	t.Helper()
	return doorQREnv{app: e.app, sessionID: sessionID}.guest(t, "", body)
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

func TestGuestCheckInFailuresAreBudgetedPerClient(t *testing.T) {
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
	if status, _ := e.guest(t, client, junk); status != fiber.StatusNotFound {
		t.Fatalf("tenth failure %d", status)
	}
	if status, code := e.guest(t, client, junk); status != fiber.StatusTooManyRequests || code != "guest_check_in_rate_limited" {
		t.Fatalf("eleventh %d %q", status, code)
	}
	// Another address has its own budget, and the token was not used up by
	// the junk.
	if status, _ := e.guest(t, "203.0.113.8", junk); status != fiber.StatusNotFound {
		t.Fatalf("other client %d", status)
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

// productionWindow is qr mode with the default Session window (12 hours each
// side), as in production.
var productionWindow = doorqr.Config{Mode: doorqr.ModeQR, SessionGrace: -1}

// TestStaticAttendanceQR: door staff get one fixed QR per Session (for a
// printout, or a screen that cannot refresh), the same every time; a guest's
// camera opens the page with it and checks in, within the Session window.
func TestStaticAttendanceQR(t *testing.T) {
	t.Parallel()
	e := newDoorQREnvWith(t, nil, productionWindow)
	start := time.Now().Add(time.Hour).Truncate(time.Second)
	end := start.Add(30 * time.Minute)
	sessionID := e.timedSession(t, start, end)

	member := groupToken(t, e.keys, memberSub, "/UYELER/ARGE/GAMELAB")
	if got := e.mintStatic(t, member, sessionID); got.status != fiber.StatusForbidden {
		t.Fatalf("member mint %d", got.status)
	}
	minted := e.mintStatic(t, e.yk, sessionID)
	if minted.status != fiber.StatusCreated {
		t.Fatalf("mint %d %v", minted.status, minted.body)
	}
	token, _ := minted.body["token"].(string)
	if !strings.HasPrefix(token, "v1.s.") || minted.body["use"] != "static" ||
		minted.body["maxUses"] != float64(0) || minted.body["refreshAfterSeconds"] != float64(0) {
		t.Fatalf("static pass %v", minted.body)
	}
	if want := start.Add(-doorqr.DefaultSessionGrace).UTC().Format(time.RFC3339); minted.body["validFrom"] != want {
		t.Fatalf("validFrom %v, want %s", minted.body["validFrom"], want)
	}
	if want := end.Add(doorqr.DefaultSessionGrace).UTC().Format(time.RFC3339); minted.body["validUntil"] != want || minted.body["expiresAt"] != want {
		t.Fatalf("validUntil %v expiresAt %v, want %s", minted.body["validUntil"], minted.body["expiresAt"], want)
	}
	if again := e.mintStatic(t, e.yk, sessionID); again.body["token"] != token {
		t.Fatalf("not the same QR: %v", again.body["token"])
	}
	if svg := sendJSON(t, e.app, e.yk, fiber.MethodPost, "/v1/sessions/"+sessionID+"/door-qr?use=static&svg=1", ""); svg.status != fiber.StatusCreated ||
		!strings.HasPrefix(svg.body["svg"].(string), "<svg") {
		t.Fatalf("svg %d", svg.status)
	}
	if status, code := e.guestAt(t, sessionID, `{"email":"ada@example.com","doorToken":"`+token+`"}`); status != fiber.StatusCreated {
		t.Fatalf("check-in %d %q", status, code)
	}
	if status, code := e.guestAt(t, sessionID, `{"email":"ada@example.com","doorToken":"`+token+`"}`); status != fiber.StatusConflict {
		t.Fatalf("again %d %q", status, code)
	}
	// Another Session's static QR does not open this one.
	otherID := e.timedSession(t, start, end)
	other := e.mintStatic(t, e.yk, otherID).body["token"].(string)
	if status, code := e.guestAt(t, sessionID, `{"email":"ada@example.com","doorToken":"`+other+`"}`); status != fiber.StatusForbidden || code != "door_qr_invalid" {
		t.Fatalf("other session's QR %d %q", status, code)
	}
}

// TestStaticAttendanceQRWindow: it can be printed days ahead, but checks
// nobody in outside the Session window; a Session without times gets none.
func TestStaticAttendanceQRWindow(t *testing.T) {
	t.Parallel()
	e := newDoorQREnvWith(t, nil, productionWindow)
	start := time.Now().Add(72 * time.Hour)
	later := e.timedSession(t, start, start.Add(time.Hour))
	minted := e.mintStatic(t, e.yk, later)
	if minted.status != fiber.StatusCreated {
		t.Fatalf("mint ahead %d %v", minted.status, minted.body)
	}
	token := minted.body["token"].(string)
	if status, code := e.guestAt(t, later, `{"email":"ada@example.com","doorToken":"`+token+`"}`); status != fiber.StatusForbidden || code != "session_closed" {
		t.Fatalf("before the window %d %q", status, code)
	}
	if got := e.mintStatic(t, e.yk, e.sessionID); got.status != fiber.StatusBadRequest || got.body["code"] != "session_times_required" {
		t.Fatalf("untimed session %d %v", got.status, got.body)
	}
}

// TestStaticAttendanceQROff: DOOR_QR_STATIC=off refuses the static QR; the
// rotating one is untouched.
func TestStaticAttendanceQROff(t *testing.T) {
	t.Parallel()
	e := newDoorQREnvWith(t, nil, doorqr.Config{Mode: doorqr.ModeQR, SessionGrace: -1, StaticOff: true})
	start := time.Now().Add(time.Hour)
	sessionID := e.timedSession(t, start, start.Add(time.Hour))
	if got := e.mintStatic(t, e.yk, sessionID); got.status != fiber.StatusBadRequest || got.body["code"] != "door_qr_static_off" {
		t.Fatalf("static off %d %v", got.status, got.body)
	}
	if got := sendJSON(t, e.app, e.yk, fiber.MethodPost, "/v1/sessions/"+sessionID+"/door-qr?use=display", ""); got.status != fiber.StatusCreated {
		t.Fatalf("display %d", got.status)
	}
}
