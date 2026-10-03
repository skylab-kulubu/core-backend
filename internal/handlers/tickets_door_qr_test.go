package handlers

import (
	"encoding/json"
	"io"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authn"
	"github.com/skylab-kulubu/core-backend/internal/doorqr"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
)

func TestDoorQRHTTPMintThenGuestCheckIn(t *testing.T) {
	t.Parallel()
	events := event.NewMemoryStore()
	tickets := ticket.NewMemoryStore()
	gate := doorqr.NewGate([]byte("handler door qr key, handler doo"), doorqr.Config{Mode: doorqr.ModeQR})
	staff := uuid.MustParse("99999999-9999-9999-9999-999999999999")
	ev, err := events.Create(t.Context(), event.Event{Name: "Hack", Location: "YTÜ", OwnerTeam: "WEBLAB", DoorStaffIDs: []uuid.UUID{staff}})
	if err != nil {
		t.Fatal(err)
	}
	day, err := events.CreateDay(t.Context(), event.Day{EventID: ev.ID, Name: "Day 1"})
	if err != nil {
		t.Fatal(err)
	}
	sess, err := events.CreateSession(t.Context(), event.Session{EventDayID: day.ID, Title: "Opening", SpeakerName: "Ada", SessionType: "PRESENTATION"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tickets.Create(t.Context(), ticket.Ticket{
		EventID: ev.ID, TicketType: ticket.Guest, GuestEmail: "ada@example.com", GuestFirstName: "Ada", GuestLastName: "Lovelace",
	}); err != nil {
		t.Fatal(err)
	}
	mintPath := "/v1/sessions/" + sess.ID.String() + "/door-qr"
	guestPath := "/v1/sessions/" + sess.ID.String() + "/check-in/guest"

	stranger := ticketApp(t, authn.Identity{ID: uuid.New()}, events, tickets, gate)
	resp, err := stranger.Test(httptest.NewRequest(fiber.MethodPost, mintPath, nil))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusForbidden {
		t.Fatalf("stranger mint %d", resp.StatusCode)
	}
	anonymous := ticketApp(t, authn.Identity{}, events, tickets, gate)
	resp, err = anonymous.Test(httptest.NewRequest(fiber.MethodPost, mintPath, nil))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusUnauthorized {
		t.Fatalf("anonymous mint %d", resp.StatusCode)
	}

	door := ticketApp(t, authn.Identity{ID: staff}, events, tickets, gate)
	resp, err = door.Test(httptest.NewRequest(fiber.MethodPost, mintPath, nil))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("mint %d %s", resp.StatusCode, body)
	}
	if got := resp.Header.Get(fiber.HeaderCacheControl); got != "no-store" {
		t.Fatalf("cache-control %q", got)
	}
	var pass struct {
		Token               string `json:"token"`
		URL                 string `json:"url"`
		SessionID           string `json:"sessionId"`
		EventID             string `json:"eventId"`
		ExpiresAt           string `json:"expiresAt"`
		RefreshAfterSeconds int    `json:"refreshAfterSeconds"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&pass); err != nil {
		t.Fatal(err)
	}
	if pass.Token == "" || pass.SessionID != sess.ID.String() || pass.EventID != ev.ID.String() || pass.ExpiresAt == "" || pass.RefreshAfterSeconds != 30 {
		t.Fatalf("pass %+v", pass)
	}
	u, err := url.Parse(pass.URL)
	if err != nil || u.Query().Get(doorqr.QueryParam) != pass.Token {
		t.Fatalf("url %q", pass.URL)
	}
	var plain map[string]any
	resp, err = door.Test(httptest.NewRequest(fiber.MethodPost, mintPath, nil))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.NewDecoder(resp.Body).Decode(&plain); err != nil {
		t.Fatal(err)
	}
	if _, ok := plain["svg"]; ok {
		t.Fatal("svg without ?svg=1")
	}
	var drawn struct {
		Token string `json:"token"`
		SVG   string `json:"svg"`
	}
	resp, err = door.Test(httptest.NewRequest(fiber.MethodPost, mintPath+"?svg=1", nil))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.NewDecoder(resp.Body).Decode(&drawn); err != nil {
		t.Fatal(err)
	}
	if drawn.Token == "" || !strings.HasPrefix(drawn.SVG, "<svg") {
		t.Fatalf("svg answer %.80q", drawn.SVG)
	}

	guest := func(body string) (int, string) {
		t.Helper()
		req := httptest.NewRequest(fiber.MethodPost, guestPath, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := anonymous.Test(req)
		if err != nil {
			t.Fatal(err)
		}
		var problem struct {
			Code string `json:"code"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&problem)
		return resp.StatusCode, problem.Code
	}
	if status, code := guest(`{"email":"ada@example.com"}`); status != fiber.StatusForbidden || code != "door_qr_required" {
		t.Fatalf("no token %d %q", status, code)
	}
	if status, code := guest(`{"email":"ada@example.com","doorToken":"x.y.z"}`); status != fiber.StatusForbidden || code != "door_qr_invalid" {
		t.Fatalf("bad token %d %q", status, code)
	}
	if status, _ := guest(`{"email":"ada@example.com","doorToken":"` + pass.Token + `"}`); status != fiber.StatusCreated {
		t.Fatalf("good token %d", status)
	}
}
