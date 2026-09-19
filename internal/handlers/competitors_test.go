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
	app.Get("/v1/competitors/leaderboard/team/:ownerTeam", h.LeaderboardByTeam)
	app.Get("/v1/competitors/leaderboard/season/:seasonId/team/:ownerTeam", h.LeaderboardBySeason)
	app.Get("/v1/competitors/user/:userId", h.ListByUser)
	app.Get("/v1/competitors/team/:ownerTeam", h.ListByOwnerTeam)
	app.Get("/v1/competitors/:id", h.Get)
	app.Post("/v1/competitors", h.Create)
	app.Put("/v1/competitors/:id", h.Update)
	app.Delete("/v1/competitors/:id", h.Delete)
	app.Post("/v1/competitors/:id/reinstate", h.Reinstate)
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
	if created.Event == nil || created.Event.Name != "Hack" || created.EventID != ev.ID {
		t.Fatalf("embedded event %+v", created.Event)
	}

	resp, err = app.Test(httptest.NewRequest(fiber.MethodGet, "/v1/competitors/me", nil))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("me status %d", resp.StatusCode)
	}

	resp, err = app.Test(httptest.NewRequest(fiber.MethodGet, "/v1/events/"+ev.ID.String()+"/competitors", nil))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("own list status %d", resp.StatusCode)
	}
	var listed []competitor.Competitor
	if err := json.NewDecoder(resp.Body).Decode(&listed); err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].ID != created.ID {
		t.Fatalf("listed %+v", listed)
	}

	public := competitorApp(t, authn.Identity{}, events, comps)
	resp, err = public.Test(httptest.NewRequest(fiber.MethodGet, "/v1/events/"+ev.ID.String()+"/competitors", nil))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusUnauthorized {
		t.Fatalf("anon list %d", resp.StatusCode)
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
	if resp.StatusCode != fiber.StatusUnauthorized {
		t.Fatalf("anon winner %d", resp.StatusCode)
	}

	resp, err = app.Test(httptest.NewRequest(fiber.MethodGet, "/v1/events/"+ev.ID.String()+"/competitors/winner", nil))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("winner status %d body %s", resp.StatusCode, body)
	}

	resp, err = public.Test(httptest.NewRequest(fiber.MethodGet, "/v1/competitors/leaderboard/team/WEBLAB", nil))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusUnauthorized {
		t.Fatalf("anon leaderboard %d", resp.StatusCode)
	}

	resp, err = app.Test(httptest.NewRequest(fiber.MethodGet, "/v1/competitors/leaderboard/team/WEBLAB", nil))
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

func TestCompetitorDumpAndUserRoutesRequireAuthz(t *testing.T) {
	t.Parallel()
	events := event.NewMemoryStore()
	comps := competitor.NewMemoryStore(events)
	ev, err := events.Create(t.Context(), event.Event{Name: "Hack", Location: "YTÜ", OwnerTeam: "WEBLAB", Active: true})
	if err != nil {
		t.Fatal(err)
	}
	owner := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	other := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	created, err := comps.Create(t.Context(), competitor.Competitor{UserID: owner, EventID: ev.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := comps.Create(t.Context(), competitor.Competitor{UserID: other, EventID: ev.ID}); err != nil {
		t.Fatal(err)
	}

	anon := competitorApp(t, authn.Identity{}, events, comps)
	resp, err := anon.Test(httptest.NewRequest(fiber.MethodGet, "/v1/competitors", nil))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusUnauthorized {
		t.Fatalf("anon dump %d", resp.StatusCode)
	}
	resp, err = anon.Test(httptest.NewRequest(fiber.MethodGet, "/v1/competitors/"+created.ID.String(), nil))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusUnauthorized {
		t.Fatalf("anon get %d", resp.StatusCode)
	}
	resp, err = anon.Test(httptest.NewRequest(fiber.MethodGet, "/v1/competitors/user/"+owner.String(), nil))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusUnauthorized {
		t.Fatalf("anon user %d", resp.StatusCode)
	}

	member := competitorApp(t, authn.Identity{
		ID:     uuid.MustParse("33333333-3333-3333-3333-333333333333"),
		Groups: []string{"/UYELER/ARGE/SKYSEC"},
	}, events, comps)
	resp, err = member.Test(httptest.NewRequest(fiber.MethodGet, "/v1/competitors", nil))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusForbidden {
		t.Fatalf("member dump %d", resp.StatusCode)
	}
	resp, err = member.Test(httptest.NewRequest(fiber.MethodGet, "/v1/competitors/user/"+owner.String(), nil))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusForbidden {
		t.Fatalf("member other user %d", resp.StatusCode)
	}

	self := competitorApp(t, authn.Identity{ID: owner}, events, comps)
	resp, err = self.Test(httptest.NewRequest(fiber.MethodGet, "/v1/competitors/user/"+owner.String(), nil))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("self user %d", resp.StatusCode)
	}
	var mine []competitor.Competitor
	if err := json.NewDecoder(resp.Body).Decode(&mine); err != nil {
		t.Fatal(err)
	}
	if len(mine) != 1 || mine[0].UserID != owner {
		t.Fatalf("mine %+v", mine)
	}

	yk := competitorApp(t, authn.Identity{
		ID:     uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"),
		Groups: []string{"/UYELER/YK"},
	}, events, comps)
	resp, err = yk.Test(httptest.NewRequest(fiber.MethodGet, "/v1/competitors", nil))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("yk dump %d %s", resp.StatusCode, body)
	}
	var all []competitor.Competitor
	if err := json.NewDecoder(resp.Body).Decode(&all); err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("all %+v", all)
	}
}
