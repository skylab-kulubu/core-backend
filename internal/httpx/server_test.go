package httpx_test

import (
	"encoding/json"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authn"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/competitor"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/httpx"
	"github.com/skylab-kulubu/core-backend/internal/identity"
	"github.com/skylab-kulubu/core-backend/internal/media"
	"github.com/skylab-kulubu/core-backend/internal/season"
	"github.com/skylab-kulubu/core-backend/internal/shorturl"
	"github.com/skylab-kulubu/core-backend/internal/testauth"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

func memoryApp(parse ...func(string) (authn.Identity, error)) *fiber.App {
	az := authz.NewAuthorizer(authz.DefaultPolicy())
	users := user.NewMemoryStore()
	events := event.NewMemoryStore()
	deps := httpx.Deps{
		Users:       user.NewService(users),
		Identity:    identity.NewService(identity.NewMemory(), users, az),
		Events:      event.NewService(events, az),
		Seasons:     season.NewService(season.NewMemoryStore(), az),
		Tickets:     ticket.NewService(ticket.NewMemoryStore(), events, az),
		Competitors: competitor.NewService(competitor.NewMemoryStore(events), events, az),
		Media:       media.NewService(media.NewMemoryStore(), media.NewMemoryBlob(), az, ""),
		URLs:        shorturl.NewService(shorturl.NewMemoryStore(), az),
	}
	if len(parse) > 0 {
		deps.ParseToken = parse[0]
	}
	return httpx.New(deps)
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
	keys := testauth.New(t)
	app := memoryApp(keys.Parse())
	id := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	req := httptest.NewRequest(fiber.MethodGet, "/v1/users/me", nil)
	req.Header.Set("Authorization", "Bearer "+keys.Token(t, jwt.MapClaims{
		"sub": id.String(), "email": "yk@example.com", "given_name": "Y", "family_name": "K", "groups": []string{"/UYELER/YK"},
	}))
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
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "application/problem+json") {
		t.Fatalf("content-type %s", ct)
	}
}

func TestUnsetJWKSRejectsAnyBearer(t *testing.T) {
	t.Parallel()
	app := memoryApp()
	id := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	keys := testauth.New(t)
	req := httptest.NewRequest(fiber.MethodGet, "/v1/users/me", nil)
	req.Header.Set("Authorization", "Bearer "+keys.Token(t, jwt.MapClaims{"sub": id.String(), "email": "yk@example.com"}))
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusUnauthorized {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

func TestVerifiedBearerAcceptsSignedJWT(t *testing.T) {
	t.Parallel()
	keys := testauth.New(t)
	app := memoryApp(keys.Parse())
	id := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	req := httptest.NewRequest(fiber.MethodGet, "/v1/users/me", nil)
	req.Header.Set("Authorization", "Bearer "+keys.Token(t, jwt.MapClaims{"sub": id.String(), "email": "yk@example.com"}))
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d body %s", resp.StatusCode, body)
	}
}

func TestVerifiedBearerRejectsUnsigned(t *testing.T) {
	t.Parallel()
	keys := testauth.New(t)
	app := memoryApp(keys.Parse())
	id := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	header := "eyJhbGciOiJub25lIn0"
	body := "eyJzdWIiOiIxMTExMTExMS0xMTExLTExMTEtMTExMS0xMTExMTExMTExMTEiLCJhdWQiOiJjb3JlIn0"
	_ = id
	req := httptest.NewRequest(fiber.MethodGet, "/v1/users/me", nil)
	req.Header.Set("Authorization", "Bearer "+header+"."+body+".x")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusUnauthorized {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

func TestVerifiedBearerRejectsWrongAudience(t *testing.T) {
	t.Parallel()
	keys := testauth.New(t)
	app := memoryApp(keys.Parse())
	id := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	req := httptest.NewRequest(fiber.MethodGet, "/v1/users/me", nil)
	req.Header.Set("Authorization", "Bearer "+keys.Token(t, jwt.MapClaims{"sub": id.String(), "aud": "account"}))
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusUnauthorized {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var problem map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&problem); err != nil {
		t.Fatal(err)
	}
	if problem["title"] == nil || problem["status"] == nil || problem["type"] == nil || problem["detail"] == nil || problem["instance"] == nil {
		t.Fatalf("problem %+v", problem)
	}
}

func TestVerifiedBearerRejectsWrongIssuer(t *testing.T) {
	t.Parallel()
	keys := testauth.New(t)
	app := memoryApp(keys.Parse())
	id := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	req := httptest.NewRequest(fiber.MethodGet, "/v1/users/me", nil)
	req.Header.Set("Authorization", "Bearer "+keys.Token(t, jwt.MapClaims{
		"sub": id.String(), "iss": "https://other.example/realms/e-skylab",
	}))
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusUnauthorized {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

func TestVerifiedBearerRejectsMissingAudience(t *testing.T) {
	t.Parallel()
	keys := testauth.New(t)
	app := memoryApp(keys.Parse())
	id := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	tok := keys.Sign(t, jwt.MapClaims{
		"sub": id.String(),
		"iss": keys.Issuer,
		"exp": time.Now().Add(time.Hour).Unix(),
	})
	req := httptest.NewRequest(fiber.MethodGet, "/v1/users/me", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusUnauthorized {
		t.Fatalf("status %d", resp.StatusCode)
	}
}
