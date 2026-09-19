package handlers

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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
	app.Get("/v1/event-days/:id/current-session", sch.CurrentSession)
	app.Post("/v1/sessions", sch.CreateSession)
	app.Get("/v1/sessions/:id/qr", sch.SessionQR)
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

func TestCurrentSessionAndQRHTTP(t *testing.T) {
	t.Parallel()
	events := event.NewMemoryStore()
	leader := authn.Identity{ID: uuid.MustParse("22222222-2222-2222-2222-222222222222"), Groups: []string{"/UYELER/ARGE/WEBLAB/LIDERLER"}}
	app := scheduleApp(t, leader, events, season.NewMemoryStore())
	req := httptest.NewRequest(fiber.MethodPost, "/v1/events", strings.NewReader(
		`{"name":"Hack","location":"YTÜ","ownerTeam":"WEBLAB"}`,
	))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("event %d %s", resp.StatusCode, body)
	}
	var ev event.Event
	if err := json.NewDecoder(resp.Body).Decode(&ev); err != nil {
		t.Fatal(err)
	}
	dayReq := httptest.NewRequest(fiber.MethodPost, "/v1/event-days", strings.NewReader(
		`{"eventId":"`+ev.ID.String()+`","name":"Day 1"}`,
	))
	dayReq.Header.Set("Content-Type", "application/json")
	resp, err = app.Test(dayReq)
	if err != nil {
		t.Fatal(err)
	}
	var day event.Day
	if err := json.NewDecoder(resp.Body).Decode(&day); err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 9, 17, 10, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)
	body := `{"eventDayId":"` + day.ID.String() + `","title":"Talk","speakerName":"Ada","sessionType":"PRESENTATION","startTime":"` + start.Format(time.RFC3339) + `","endTime":"` + end.Format(time.RFC3339) + `"}`
	sessReq := httptest.NewRequest(fiber.MethodPost, "/v1/sessions", strings.NewReader(body))
	sessReq.Header.Set("Content-Type", "application/json")
	resp, err = app.Test(sessReq)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusCreated {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("session %d %s", resp.StatusCode, raw)
	}
	var sess event.Session
	if err := json.NewDecoder(resp.Body).Decode(&sess); err != nil {
		t.Fatal(err)
	}

	curPath := "/v1/event-days/" + day.ID.String() + "/current-session?at=" + start.Add(20*time.Minute).Format(time.RFC3339)
	resp, err = app.Test(httptest.NewRequest(fiber.MethodGet, curPath, nil))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("current %d %s", resp.StatusCode, raw)
	}
	var cur event.Current
	if err := json.NewDecoder(resp.Body).Decode(&cur); err != nil {
		t.Fatal(err)
	}
	if !cur.ClockMatch || cur.Session == nil || cur.Session.ID != sess.ID {
		t.Fatalf("current %+v", cur)
	}

	qrResp, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/v1/sessions/"+sess.ID.String()+"/qr", nil))
	if err != nil {
		t.Fatal(err)
	}
	if qrResp.StatusCode != fiber.StatusOK {
		raw, _ := io.ReadAll(qrResp.Body)
		t.Fatalf("qr %d %s", qrResp.StatusCode, raw)
	}
	if ct := qrResp.Header.Get("Content-Type"); !strings.Contains(ct, "image/png") {
		t.Fatalf("content-type %s", ct)
	}
}

func TestSessionQRLogoOverlaysClubMark(t *testing.T) {
	t.Parallel()
	events := event.NewMemoryStore()
	leader := authn.Identity{ID: uuid.MustParse("22222222-2222-2222-2222-222222222222"), Groups: []string{"/UYELER/ARGE/WEBLAB/LIDERLER"}}
	app := scheduleApp(t, leader, events, season.NewMemoryStore())
	req := httptest.NewRequest(fiber.MethodPost, "/v1/events", strings.NewReader(
		`{"name":"Hack","location":"YTÜ","ownerTeam":"WEBLAB"}`,
	))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("event %d %s", resp.StatusCode, body)
	}
	var ev event.Event
	if err := json.NewDecoder(resp.Body).Decode(&ev); err != nil {
		t.Fatal(err)
	}
	dayReq := httptest.NewRequest(fiber.MethodPost, "/v1/event-days", strings.NewReader(
		`{"eventId":"`+ev.ID.String()+`","name":"Day 1"}`,
	))
	dayReq.Header.Set("Content-Type", "application/json")
	resp, err = app.Test(dayReq)
	if err != nil {
		t.Fatal(err)
	}
	var day event.Day
	if err := json.NewDecoder(resp.Body).Decode(&day); err != nil {
		t.Fatal(err)
	}
	sessReq := httptest.NewRequest(fiber.MethodPost, "/v1/sessions", strings.NewReader(
		`{"eventDayId":"`+day.ID.String()+`","title":"Talk","speakerName":"Ada","sessionType":"PRESENTATION"}`,
	))
	sessReq.Header.Set("Content-Type", "application/json")
	resp, err = app.Test(sessReq)
	if err != nil {
		t.Fatal(err)
	}
	var sess event.Session
	if err := json.NewDecoder(resp.Body).Decode(&sess); err != nil {
		t.Fatal(err)
	}

	path := "/v1/sessions/" + sess.ID.String() + "/qr"
	plainResp, err := app.Test(httptest.NewRequest(fiber.MethodGet, path, nil))
	if err != nil {
		t.Fatal(err)
	}
	plain, err := io.ReadAll(plainResp.Body)
	if err != nil {
		t.Fatal(err)
	}
	logoResp, err := app.Test(httptest.NewRequest(fiber.MethodGet, path+"?logo=1&size=256", nil))
	if err != nil {
		t.Fatal(err)
	}
	if logoResp.StatusCode != fiber.StatusOK {
		raw, _ := io.ReadAll(logoResp.Body)
		t.Fatalf("logo qr %d %s", logoResp.StatusCode, raw)
	}
	if ct := logoResp.Header.Get("Content-Type"); !strings.Contains(ct, "image/png") {
		t.Fatalf("content-type %s", ct)
	}
	withLogo, err := io.ReadAll(logoResp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if len(withLogo) < 8 || string(withLogo[:4]) != "\x89PNG" {
		t.Fatalf("not png len=%d", len(withLogo))
	}
	if bytes.Equal(plain, withLogo) {
		t.Fatal("logo overlay should change the png")
	}
}
