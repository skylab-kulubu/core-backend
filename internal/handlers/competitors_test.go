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
	"github.com/skylab-kulubu/core-backend/internal/competitor"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

func competitorApp(t *testing.T, ident authn.Identity, events event.Store, comps competitor.Store) *fiber.App {
	t.Helper()
	svc := competitor.NewService(comps, events, authz.NewAuthorizer(authz.DefaultPolicy()))
	h := NewCompetitorHandler(svc)
	app := fiber.New()
	app.Use(func(c fiber.Ctx) error {
		if ident.ID != uuid.Nil || len(ident.Groups) > 0 {
			c.Locals(authn.LocalsIdentity, ident)
		}
		return c.Next()
	})
	app.Get("/v1/competitors", h.List)
	app.Get("/v1/competitors/me", h.Mine)
	app.Get("/v1/competitors/leaderboard/type/:eventType", h.LeaderboardByType)
	app.Get("/v1/competitors/leaderboard/season/:seasonId/type/:eventType", h.LeaderboardBySeason)
	app.Get("/v1/competitors/user/:userId", h.ListByUser)
	app.Get("/v1/competitors/team/:ownerTeam", h.ListByOwnerTeam)
	app.Get("/v1/competitors/:id", h.Get)
	app.Post("/v1/competitors", h.Create)
	app.Put("/v1/competitors/:id", h.Update)
	app.Delete("/v1/competitors/:id", h.Delete)
	app.Get("/v1/events/:eventId/competitors", h.ListByEvent)
	app.Get("/v1/events/:eventId/competitors/winner", h.Winner)
	return app
}

func TestCompetitorSelfRegisterAndPublicReadHTTP(t *testing.T) {
	t.Parallel()
	events := event.NewMemoryStore()
	comps := competitor.NewMemoryStore(events)
	ev, err := events.Create(t.Context(), event.Event{Name: "Hack", Location: "YTÜ", OwnerTeam: "WEBLAB", Active: true})
	if err != nil {
		t.Fatal(err)
	}
	userID := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	ident := authn.Identity{ID: userID, Profile: user.Profile{Email: "ada@example.com"}}
	app := competitorApp(t, ident, events, comps)

	req := httptest.NewRequest(fiber.MethodPost, "/v1/competitors", strings.NewReader(
		`{"userId":"`+userID.String()+`","eventId":"`+ev.ID.String()+`","score":40}`,
	))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("create status %d body %s", resp.StatusCode, body)
	}
	var created competitor.Competitor
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	if created.Score != nil {
		t.Fatalf("self register kept score %+v", created)
	}

	resp, err = app.Test(httptest.NewRequest(fiber.MethodGet, "/v1/competitors/me", nil))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("me status %d", resp.StatusCode)
	}

	public := competitorApp(t, authn.Identity{}, events, comps)
	resp, err = public.Test(httptest.NewRequest(fiber.MethodGet, "/v1/events/"+ev.ID.String()+"/competitors", nil))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("list status %d", resp.StatusCode)
	}
	var listed []competitor.Competitor
	if err := json.NewDecoder(resp.Body).Decode(&listed); err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].ID != created.ID {
		t.Fatalf("listed %+v", listed)
	}
}

func TestCompetitorStaffScoreAndForbiddenHTTP(t *testing.T) {
	t.Parallel()
	events := event.NewMemoryStore()
	comps := competitor.NewMemoryStore(events)
	ev, err := events.Create(t.Context(), event.Event{Name: "Hack", Location: "YTÜ", OwnerTeam: "WEBLAB", Active: true})
	if err != nil {
		t.Fatal(err)
	}
	userID := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	created, err := comps.Create(t.Context(), competitor.Competitor{UserID: userID, EventID: ev.ID})
	if err != nil {
		t.Fatal(err)
	}

	stranger := authn.Identity{ID: uuid.MustParse("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"), Groups: []string{"/UYELER/ARGE/SKYSEC"}}
	app := competitorApp(t, stranger, events, comps)
	req := httptest.NewRequest(fiber.MethodPut, "/v1/competitors/"+created.ID.String(), strings.NewReader(
		`{"userId":"`+userID.String()+`","eventId":"`+ev.ID.String()+`","score":9,"isWinner":true}`,
	))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusForbidden {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("stranger status %d body %s", resp.StatusCode, body)
	}

	leader := authn.Identity{ID: uuid.MustParse("cccccccc-cccc-cccc-cccc-cccccccccccc"), Groups: []string{"/UYELER/ARGE/WEBLAB/LIDERLER"}}
	app = competitorApp(t, leader, events, comps)
	req = httptest.NewRequest(fiber.MethodPut, "/v1/competitors/"+created.ID.String(), strings.NewReader(
		`{"userId":"`+userID.String()+`","eventId":"`+ev.ID.String()+`","score":9,"isWinner":true}`,
	))
	req.Header.Set("Content-Type", "application/json")
	resp, err = app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("leader status %d body %s", resp.StatusCode, body)
	}

	public := competitorApp(t, authn.Identity{}, events, comps)
	resp, err = public.Test(httptest.NewRequest(fiber.MethodGet, "/v1/events/"+ev.ID.String()+"/competitors/winner", nil))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("winner status %d body %s", resp.StatusCode, body)
	}

	resp, err = public.Test(httptest.NewRequest(fiber.MethodGet, "/v1/competitors/leaderboard/type/WEBLAB", nil))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("leaderboard status %d", resp.StatusCode)
	}
	var board []competitor.LeaderboardEntry
	if err := json.NewDecoder(resp.Body).Decode(&board); err != nil {
		t.Fatal(err)
	}
	if len(board) != 1 || board[0].UserID != userID || board[0].TotalScore != 9 || board[0].Rank != 1 {
		t.Fatalf("board %+v", board)
	}
}
