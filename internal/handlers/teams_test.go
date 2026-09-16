package handlers

import (
	"encoding/json"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/identity"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

func teamApp(t *testing.T, dir *identity.Memory) *fiber.App {
	t.Helper()
	svc := identity.NewService(dir, user.NewMemoryStore(), authz.NewAuthorizer(authz.DefaultPolicy()))
	h := NewTeamHandler(svc)
	app := fiber.New()
	app.Get("/v1/teams", h.List)
	app.Get("/v1/teams/:team/members", h.Members)
	app.Get("/v1/teams/:team/leaders", h.Leaders)
	return app
}

func seedPublicTeam(t *testing.T, dir *identity.Memory) {
	t.Helper()
	memberID := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaa11")
	leaderID := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaa12")
	dir.PutGroup(identity.Group{
		ID:         "g-weblab",
		Name:       "WEBLAB",
		Path:       "/UYELER/ARGE/WEBLAB",
		Attributes: map[string]string{"public_listing": "true"},
	})
	dir.PutGroup(identity.Group{ID: "g-weblab-l", Name: "LIDERLER", Path: "/UYELER/ARGE/WEBLAB/LIDERLER"})
	dir.PutGroup(identity.Group{ID: "g-yk", Name: "YK", Path: "/UYELER/YK"})
	dir.PutUser(identity.Person{ID: memberID, Email: "secret-member@example.com", FirstName: "Ada", LastName: "Member"})
	dir.PutUser(identity.Person{ID: leaderID, Email: "secret-leader@example.com", FirstName: "Grace", LastName: "Leader"})
	ctx := t.Context()
	if err := dir.AddMember(ctx, "g-weblab", memberID); err != nil {
		t.Fatal(err)
	}
	if err := dir.AddMember(ctx, "g-weblab-l", leaderID); err != nil {
		t.Fatal(err)
	}
}

func TestPublicTeamMembersUnauthenticated(t *testing.T) {
	t.Parallel()
	dir := identity.NewMemory()
	seedPublicTeam(t, dir)
	app := teamApp(t, dir)

	resp, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/v1/teams/WEBLAB/members", nil))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d body %s", resp.StatusCode, body)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "example.com") || strings.Contains(string(body), "aaaaaaaa-aaaa") {
		t.Fatalf("pii leaked: %s", body)
	}
	var roster identity.Roster
	if err := json.Unmarshal(body, &roster); err != nil {
		t.Fatal(err)
	}
	if roster.Count != 2 {
		t.Fatalf("count %d", roster.Count)
	}
}

func TestPublicTeamNotFoundWhenPrivate(t *testing.T) {
	t.Parallel()
	dir := identity.NewMemory()
	seedPublicTeam(t, dir)
	app := teamApp(t, dir)

	resp, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/v1/teams/YK/members", nil))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusNotFound {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

func TestPublicTeamListOmitsYK(t *testing.T) {
	t.Parallel()
	dir := identity.NewMemory()
	seedPublicTeam(t, dir)
	app := teamApp(t, dir)

	resp, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/v1/teams", nil))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var teams []identity.PublicTeam
	if err := json.NewDecoder(resp.Body).Decode(&teams); err != nil {
		t.Fatal(err)
	}
	if len(teams) != 1 || teams[0].Team != "WEBLAB" {
		t.Fatalf("teams %+v", teams)
	}
}

func TestPublicLeadersEndpoint(t *testing.T) {
	t.Parallel()
	dir := identity.NewMemory()
	seedPublicTeam(t, dir)
	app := teamApp(t, dir)

	resp, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/v1/teams/WEBLAB/leaders", nil))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var roster identity.Roster
	if err := json.NewDecoder(resp.Body).Decode(&roster); err != nil {
		t.Fatal(err)
	}
	if roster.Count != 1 || !roster.Members[0].Leader {
		t.Fatalf("leaders %+v", roster)
	}
}
