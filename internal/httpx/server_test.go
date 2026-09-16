package httpx_test

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/competitor"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/httpx"
	"github.com/skylab-kulubu/core-backend/internal/identity"
	"github.com/skylab-kulubu/core-backend/internal/media"
	"github.com/skylab-kulubu/core-backend/internal/season"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

func memoryApp() *fiber.App {
	az := authz.NewAuthorizer(authz.DefaultPolicy())
	users := user.NewMemoryStore()
	events := event.NewMemoryStore()
	return httpx.New(httpx.Deps{
		Users:       user.NewService(users),
		Identity:    identity.NewService(identity.NewMemory(), users, az),
		Events:      event.NewService(events, az),
		Seasons:     season.NewService(season.NewMemoryStore(), az),
		Tickets:     ticket.NewService(ticket.NewMemoryStore(), events, az),
		Competitors: competitor.NewService(competitor.NewMemoryStore(events), events, az),
		Media:       media.NewService(media.NewMemoryStore(), media.NewMemoryBlob(), az, ""),
	})
}

func unsignedJWT(payload string) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	body := base64.RawURLEncoding.EncodeToString([]byte(payload))
	return header + "." + body + ".x"
}

func TestHealthAnonymous(t *testing.T) {
	t.Parallel()
	app := memoryApp()
	resp, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/v1/health", nil))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusNoContent {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

func TestBearerGroupsReachMe(t *testing.T) {
	t.Parallel()
	app := memoryApp()
	id := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	req := httptest.NewRequest(fiber.MethodGet, "/v1/users/me", nil)
	req.Header.Set("Authorization", "Bearer "+unsignedJWT(
		`{"sub":"`+id.String()+`","email":"yk@example.com","given_name":"Y","family_name":"K","groups":["/UYELER/YK"]}`,
	))
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d body %s", resp.StatusCode, body)
	}
	var got user.User
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.ID != id || got.Email != "yk@example.com" {
		t.Fatalf("got %+v", got)
	}
}

func TestInvalidBearerIs401Problem(t *testing.T) {
	t.Parallel()
	app := memoryApp()
	req := httptest.NewRequest(fiber.MethodGet, "/v1/users/me", nil)
	req.Header.Set("Authorization", "Bearer not-a-jwt")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusUnauthorized {
		t.Fatalf("status %d", resp.StatusCode)
	}
}
