package httpx_test

import (
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/doorqr"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/httpx"
	"github.com/skylab-kulubu/core-backend/internal/testauth"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
)

// TestGuestSelfCheckInWithDoorQR runs the whole door QR flow through the
// assembled app in qr mode: the door screen mints with its token, the guest
// checks in without one.
func TestGuestSelfCheckInWithDoorQR(t *testing.T) {
	t.Parallel()
	keys := testauth.New(t)
	az := authz.NewAuthorizer(authz.DefaultPolicy())
	events := event.NewMemoryStore()
	gate := doorqr.NewGate([]byte("assembled door qr key, assembled"), doorqr.Config{Mode: doorqr.ModeQR})
	deps := memoryDeps()
	deps.ParseToken = keys.Parse()
	deps.Events = event.NewService(events, az)
	deps.Tickets = ticket.NewService(ticket.NewMemoryStore(), events, az, gate)
	deps.GuestCheckInMetrics = gate
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
	sessionID := sess.body["id"].(string)
	applied := sendJSON(t, app, yk, fiber.MethodPost, "/v1/events/"+eventID+"/applications/guest",
		`{"firstName":"Ada","lastName":"Lovelace","email":"ada@example.com"}`)
	if applied.status != fiber.StatusCreated && applied.status != fiber.StatusOK {
		t.Fatalf("guest apply %d %v", applied.status, applied.body)
	}

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
		req := httptest.NewRequest(fiber.MethodPost, "/v1/sessions/"+sessionID+"/check-in/guest", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := app.Test(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode
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
