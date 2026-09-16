package handlers

import (
	"encoding/json"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authn"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/season"
)

func scheduleApp(t *testing.T, ident authn.Identity, events event.Store, seasons season.Store) *fiber.App {
	t.Helper()
	az := authz.NewAuthorizer(authz.DefaultPolicy())
	evSvc := event.NewService(events, az)
	seSvc := season.NewService(seasons, az)
	eh := NewEventHandler(evSvc)
	sh := NewSeasonHandler(seSvc, evSvc)
	sch := NewScheduleHandler(evSvc)
	app := fiber.New()
	app.Use(func(c fiber.Ctx) error {
		if ident.ID != uuid.Nil || len(ident.Groups) > 0 {
			c.Locals(authn.LocalsIdentity, ident)
		}
		return c.Next()
	})
	app.Post("/v1/events", eh.Create)
	app.Get("/v1/seasons", sh.List)
	app.Post("/v1/seasons", sh.Create)
	app.Get("/v1/seasons/:id/events", sh.ListEvents)
	app.Post("/v1/seasons/:id/events/:eventId", sh.AssignEvent)
	app.Get("/v1/events/:eventId/days", sch.ListDays)
	app.Post("/v1/event-days", sch.CreateDay)
	app.Get("/v1/event-days/:id/sessions", sch.ListSessions)
	app.Post("/v1/sessions", sch.CreateSession)
	return app
}

func TestPublicSeasonListAndLeaderDaySession(t *testing.T) {
	t.Parallel()
	events := event.NewMemoryStore()
	seasons := season.NewMemoryStore()
	yk := authn.Identity{ID: uuid.MustParse("11111111-1111-1111-1111-111111111111"), Groups: []string{"/UYELER/YK"}}
	app := scheduleApp(t, yk, events, seasons)

	req := httptest.NewRequest(fiber.MethodPost, "/v1/seasons", strings.NewReader(`{"name":"2026-2027","active":true}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("season status %d body %s", resp.StatusCode, body)
	}
	var createdSeason season.Season
	if err := json.NewDecoder(resp.Body).Decode(&createdSeason); err != nil {
		t.Fatal(err)
	}

	public := scheduleApp(t, authn.Identity{}, events, seasons)
	resp, err = public.Test(httptest.NewRequest(fiber.MethodGet, "/v1/seasons", nil))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("public seasons %d", resp.StatusCode)
	}

	leader := authn.Identity{ID: uuid.MustParse("22222222-2222-2222-2222-222222222222"), Groups: []string{"/UYELER/ARGE/WEBLAB/LIDERLER"}}
	app = scheduleApp(t, leader, events, seasons)
	req = httptest.NewRequest(fiber.MethodPost, "/v1/events", strings.NewReader(
		`{"name":"Hack","location":"YTÜ","ownerTeam":"WEBLAB"}`,
	))
	req.Header.Set("Content-Type", "application/json")
	resp, err = app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("event status %d body %s", resp.StatusCode, body)
	}
	var ev event.Event
	if err := json.NewDecoder(resp.Body).Decode(&ev); err != nil {
		t.Fatal(err)
	}

	req = httptest.NewRequest(fiber.MethodPost, "/v1/seasons/"+createdSeason.ID.String()+"/events/"+ev.ID.String(), nil)
	resp, err = app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("assign status %d body %s", resp.StatusCode, body)
	}

	req = httptest.NewRequest(fiber.MethodPost, "/v1/event-days", strings.NewReader(
		`{"eventId":"`+ev.ID.String()+`","name":"Day 1"}`,
	))
	req.Header.Set("Content-Type", "application/json")
	resp, err = app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("day status %d body %s", resp.StatusCode, body)
	}
	var day event.Day
	if err := json.NewDecoder(resp.Body).Decode(&day); err != nil {
		t.Fatal(err)
	}

	req = httptest.NewRequest(fiber.MethodPost, "/v1/sessions", strings.NewReader(
		`{"eventDayId":"`+day.ID.String()+`","title":"Talk","speakerName":"Ada","sessionType":"PRESENTATION"}`,
	))
	req.Header.Set("Content-Type", "application/json")
	resp, err = app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("session status %d body %s", resp.StatusCode, body)
	}

	resp, err = public.Test(httptest.NewRequest(fiber.MethodGet, "/v1/events/"+ev.ID.String()+"/days", nil))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("public days %d", resp.StatusCode)
	}
}
