package handlers

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authn"
	"github.com/skylab-kulubu/core-backend/internal/competitor"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/season"
	"github.com/skylab-kulubu/core-backend/internal/shorturl"
)

func requireStatus(t *testing.T, app *fiber.App, method, path string, want int) {
	t.Helper()
	resp, err := app.Test(httptest.NewRequest(method, path, nil))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != want {
		t.Fatalf("%s %s status = %d, want %d", method, path, resp.StatusCode, want)
	}
}

func TestEventLifecycleHTTP(t *testing.T) {
	t.Parallel()
	store := event.NewMemoryStore()
	created, err := store.Create(t.Context(), event.Event{
		Name: "Hack", Location: "YTÜ", OwnerTeam: "WEBLAB", Active: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	leader := eventApp(t, weblabLeader(), store)
	path := "/v1/events/" + created.ID.String()

	requireStatus(t, leader, fiber.MethodDelete, path, fiber.StatusNoContent)
	requireStatus(t, leader, fiber.MethodDelete, path, fiber.StatusNoContent)
	requireStatus(t, leader, fiber.MethodGet, path, fiber.StatusNotFound)
	resp, err := leader.Test(httptest.NewRequest(fiber.MethodGet, "/v1/events", nil))
	if err != nil {
		t.Fatal(err)
	}
	var current []event.Event
	if err := json.NewDecoder(resp.Body).Decode(&current); err != nil {
		t.Fatal(err)
	}
	if len(current) != 0 {
		t.Fatalf("current events = %+v", current)
	}
	requireStatus(t, eventApp(t, authn.Identity{}, store), fiber.MethodGet, "/v1/events?lifecycle=inactive", fiber.StatusUnauthorized)
	requireStatus(t, leader, fiber.MethodGet, "/v1/events?lifecycle=unknown", fiber.StatusBadRequest)

	resp, err = leader.Test(httptest.NewRequest(fiber.MethodGet, "/v1/events?lifecycle=inactive", nil))
	if err != nil {
		t.Fatal(err)
	}
	var archived []event.Event
	if err := json.NewDecoder(resp.Body).Decode(&archived); err != nil {
		t.Fatal(err)
	}
	if len(archived) != 1 || archived[0].ID != created.ID || archived[0].ArchivedAt == nil {
		t.Fatalf("archived events = %+v", archived)
	}

	restorePath := path + "/restore"
	requireStatus(t, leader, fiber.MethodPost, restorePath, fiber.StatusOK)
	requireStatus(t, leader, fiber.MethodPost, restorePath, fiber.StatusOK)
	requireStatus(t, leader, fiber.MethodGet, path, fiber.StatusOK)
}

func TestScheduleLifecycleHTTP(t *testing.T) {
	t.Parallel()
	store := event.NewMemoryStore()
	seasons := season.NewMemoryStore()
	createdEvent, err := store.Create(t.Context(), event.Event{
		Name: "Hack", Location: "YTÜ", OwnerTeam: "WEBLAB", Active: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	day, err := store.CreateDay(t.Context(), event.Day{EventID: createdEvent.ID, Name: "Day 1"})
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.CreateSession(t.Context(), event.Session{
		EventDayID: day.ID, Title: "Talk", SpeakerName: "Ada", SessionType: "PRESENTATION",
	})
	if err != nil {
		t.Fatal(err)
	}
	leader := scheduleApp(t, weblabLeader(), store, seasons)
	anonymous := scheduleApp(t, authn.Identity{}, store, seasons)
	sessionPath := "/v1/sessions/" + session.ID.String()
	sessionsPath := "/v1/event-days/" + day.ID.String() + "/sessions"

	requireStatus(t, leader, fiber.MethodDelete, sessionPath, fiber.StatusNoContent)
	requireStatus(t, leader, fiber.MethodDelete, sessionPath, fiber.StatusNoContent)
	requireStatus(t, leader, fiber.MethodGet, sessionPath, fiber.StatusNotFound)
	requireStatus(t, anonymous, fiber.MethodGet, sessionsPath+"?lifecycle=inactive", fiber.StatusUnauthorized)
	resp, err := leader.Test(httptest.NewRequest(fiber.MethodGet, sessionsPath+"?lifecycle=inactive", nil))
	if err != nil {
		t.Fatal(err)
	}
	var sessions []event.Session
	if err := json.NewDecoder(resp.Body).Decode(&sessions); err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 || sessions[0].ID != session.ID || sessions[0].ArchivedAt == nil {
		t.Fatalf("archived sessions = %+v", sessions)
	}
	resp, err = leader.Test(httptest.NewRequest(fiber.MethodGet, sessionsPath, nil))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.NewDecoder(resp.Body).Decode(&sessions); err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 0 {
		t.Fatalf("current sessions = %+v", sessions)
	}
	requireStatus(t, leader, fiber.MethodPost, sessionPath+"/restore", fiber.StatusOK)
	requireStatus(t, leader, fiber.MethodPost, sessionPath+"/restore", fiber.StatusOK)

	dayPath := "/v1/event-days/" + day.ID.String()
	daysPath := "/v1/events/" + createdEvent.ID.String() + "/days"
	requireStatus(t, leader, fiber.MethodDelete, dayPath, fiber.StatusNoContent)
	requireStatus(t, leader, fiber.MethodDelete, dayPath, fiber.StatusNoContent)
	requireStatus(t, leader, fiber.MethodGet, dayPath, fiber.StatusNotFound)
	resp, err = leader.Test(httptest.NewRequest(fiber.MethodGet, daysPath+"?lifecycle=inactive", nil))
	if err != nil {
		t.Fatal(err)
	}
	var days []event.Day
	if err := json.NewDecoder(resp.Body).Decode(&days); err != nil {
		t.Fatal(err)
	}
	if len(days) != 1 || days[0].ID != day.ID || days[0].ArchivedAt == nil {
		t.Fatalf("archived days = %+v", days)
	}
	resp, err = leader.Test(httptest.NewRequest(fiber.MethodGet, daysPath, nil))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.NewDecoder(resp.Body).Decode(&days); err != nil {
		t.Fatal(err)
	}
	if len(days) != 0 {
		t.Fatalf("current days = %+v", days)
	}
	requireStatus(t, leader, fiber.MethodPost, dayPath+"/restore", fiber.StatusOK)
	requireStatus(t, leader, fiber.MethodPost, dayPath+"/restore", fiber.StatusOK)

	requireStatus(t, leader, fiber.MethodDelete, dayPath, fiber.StatusNoContent)
	if err := store.Archive(t.Context(), createdEvent.ID, nil); err != nil {
		t.Fatal(err)
	}
	requireStatus(t, leader, fiber.MethodPost, dayPath+"/restore", fiber.StatusConflict)
	requireStatus(t, leader, fiber.MethodDelete, sessionPath, fiber.StatusNoContent)
	requireStatus(t, leader, fiber.MethodPost, sessionPath+"/restore", fiber.StatusConflict)
}

func TestSeasonLifecycleHTTP(t *testing.T) {
	t.Parallel()
	events := event.NewMemoryStore()
	store := season.NewMemoryStore()
	created, err := store.Create(t.Context(), season.Season{Name: "2026-2027", Active: true})
	if err != nil {
		t.Fatal(err)
	}
	manager := scheduleApp(t, yk(), events, store)
	path := "/v1/seasons/" + created.ID.String()

	requireStatus(t, manager, fiber.MethodDelete, path, fiber.StatusNoContent)
	requireStatus(t, manager, fiber.MethodDelete, path, fiber.StatusNoContent)
	requireStatus(t, manager, fiber.MethodGet, path, fiber.StatusNotFound)
	resp, err := manager.Test(httptest.NewRequest(fiber.MethodGet, "/v1/seasons", nil))
	if err != nil {
		t.Fatal(err)
	}
	var current []season.Season
	if err := json.NewDecoder(resp.Body).Decode(&current); err != nil {
		t.Fatal(err)
	}
	if len(current) != 0 {
		t.Fatalf("current seasons = %+v", current)
	}
	requireStatus(t, scheduleApp(t, authn.Identity{}, events, store), fiber.MethodGet, "/v1/seasons?lifecycle=inactive", fiber.StatusUnauthorized)
	requireStatus(t, manager, fiber.MethodGet, "/v1/seasons?lifecycle=nope", fiber.StatusBadRequest)

	resp, err = manager.Test(httptest.NewRequest(fiber.MethodGet, "/v1/seasons?lifecycle=inactive", nil))
	if err != nil {
		t.Fatal(err)
	}
	var archived []season.Season
	if err := json.NewDecoder(resp.Body).Decode(&archived); err != nil {
		t.Fatal(err)
	}
	if len(archived) != 1 || archived[0].ID != created.ID || archived[0].ArchivedAt == nil {
		t.Fatalf("archived seasons = %+v", archived)
	}
	requireStatus(t, manager, fiber.MethodPost, path+"/restore", fiber.StatusOK)
	requireStatus(t, manager, fiber.MethodPost, path+"/restore", fiber.StatusOK)
	requireStatus(t, manager, fiber.MethodGet, path, fiber.StatusOK)
}

func TestCompetitorLifecycleHTTP(t *testing.T) {
	t.Parallel()
	events := event.NewMemoryStore()
	comps := competitor.NewMemoryStore(events)
	createdEvent, err := events.Create(t.Context(), event.Event{
		Name: "Hack", Location: "YTÜ", OwnerTeam: "WEBLAB", Active: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	created, err := comps.Create(t.Context(), competitor.Competitor{UserID: uuid.New(), EventID: createdEvent.ID})
	if err != nil {
		t.Fatal(err)
	}
	leader := competitorApp(t, weblabLeader(), events, comps)
	path := "/v1/competitors/" + created.ID.String()

	requireStatus(t, leader, fiber.MethodDelete, path, fiber.StatusNoContent)
	requireStatus(t, leader, fiber.MethodDelete, path, fiber.StatusNoContent)
	requireStatus(t, leader, fiber.MethodGet, path, fiber.StatusNotFound)
	requireStatus(t, leader, fiber.MethodGet, "/v1/competitors?lifecycle=invalid", fiber.StatusBadRequest)
	privileged := competitorApp(t, yk(), events, comps)
	resp, err := privileged.Test(httptest.NewRequest(fiber.MethodGet, "/v1/competitors", nil))
	if err != nil {
		t.Fatal(err)
	}
	var current []competitor.Competitor
	if err := json.NewDecoder(resp.Body).Decode(&current); err != nil {
		t.Fatal(err)
	}
	if len(current) != 0 {
		t.Fatalf("current competitors = %+v", current)
	}
	resp, err = leader.Test(httptest.NewRequest(fiber.MethodGet, "/v1/competitors?lifecycle=inactive", nil))
	if err != nil {
		t.Fatal(err)
	}
	var withdrawn []competitor.Competitor
	if err := json.NewDecoder(resp.Body).Decode(&withdrawn); err != nil {
		t.Fatal(err)
	}
	if len(withdrawn) != 1 || withdrawn[0].ID != created.ID || withdrawn[0].WithdrawnAt == nil {
		t.Fatalf("withdrawn competitors = %+v", withdrawn)
	}
	reinstatePath := path + "/reinstate"
	requireStatus(t, leader, fiber.MethodPost, reinstatePath, fiber.StatusOK)
	requireStatus(t, leader, fiber.MethodPost, reinstatePath, fiber.StatusOK)

	requireStatus(t, leader, fiber.MethodDelete, path, fiber.StatusNoContent)
	if err := events.Archive(t.Context(), createdEvent.ID, nil); err != nil {
		t.Fatal(err)
	}
	requireStatus(t, leader, fiber.MethodPost, reinstatePath, fiber.StatusConflict)
}

func TestShortURLLifecycleHTTP(t *testing.T) {
	t.Parallel()
	store := shorturl.NewMemoryStore()
	ownerID := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	created, err := store.Create(t.Context(), shorturl.URL{
		Alias: "club", URL: "https://skylab.com", CreatedBy: &ownerID,
	})
	if err != nil {
		t.Fatal(err)
	}
	owner := urlAppWith(t, authn.Identity{ID: ownerID, Roles: []string{"url:access"}}, store)
	path := "/v1/urls/" + created.ID.String()

	requireStatus(t, owner, fiber.MethodDelete, path, fiber.StatusNoContent)
	requireStatus(t, owner, fiber.MethodDelete, path, fiber.StatusNoContent)
	requireStatus(t, owner, fiber.MethodGet, "/v1/go/club", fiber.StatusNotFound)
	requireStatus(t, owner, fiber.MethodGet, "/v1/urls?lifecycle=invalid", fiber.StatusBadRequest)

	resp, err := owner.Test(httptest.NewRequest(fiber.MethodGet, "/v1/urls?lifecycle=inactive", nil))
	if err != nil {
		t.Fatal(err)
	}
	var disabled []shorturl.URL
	if err := json.NewDecoder(resp.Body).Decode(&disabled); err != nil {
		t.Fatal(err)
	}
	if len(disabled) != 1 || disabled[0].ID != created.ID || disabled[0].DisabledAt == nil {
		t.Fatalf("disabled urls = %+v", disabled)
	}
	resp, err = owner.Test(httptest.NewRequest(fiber.MethodGet, "/v1/urls", nil))
	if err != nil {
		t.Fatal(err)
	}
	var current []shorturl.URL
	if err := json.NewDecoder(resp.Body).Decode(&current); err != nil {
		t.Fatal(err)
	}
	if len(current) != 0 {
		t.Fatalf("current urls = %+v", current)
	}

	moderator := urlAppWith(t, authn.Identity{ID: uuid.New(), Roles: []string{"url:moderator"}}, store)
	requireStatus(t, moderator, fiber.MethodGet, "/v1/urls/all?lifecycle=inactive", fiber.StatusOK)
	restorePath := path + "/restore"
	requireStatus(t, owner, fiber.MethodPost, restorePath, fiber.StatusOK)
	requireStatus(t, owner, fiber.MethodPost, restorePath, fiber.StatusOK)
	requireStatus(t, owner, fiber.MethodGet, "/v1/go/club", fiber.StatusMovedPermanently)
}
