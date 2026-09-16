package httpx_test

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
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
	}
	if len(parse) > 0 {
		deps.ParseToken = parse[0]
	}
	return httpx.New(deps)
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

func signedJWT(t *testing.T) (token string, jwksURL string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	n := base64.RawURLEncoding.EncodeToString(key.N.Bytes())
	e := base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes())
	body, err := json.Marshal(map[string]any{
		"keys": []map[string]string{{
			"kty": "RSA", "kid": "k1", "alg": "RS256", "use": "sig", "n": n, "e": e,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	id := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
		"sub":    id.String(),
		"email":  "yk@example.com",
		"groups": []string{"/UYELER/YK"},
		"exp":    time.Now().Add(time.Hour).Unix(),
	})
	tok.Header["kid"] = "k1"
	signed, err := tok.SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	return signed, srv.URL
}

func TestVerifiedBearerAcceptsSignedJWT(t *testing.T) {
	t.Parallel()
	signed, jwksURL := signedJWT(t)
	v := authn.NewJWKS(jwksURL)
	app := memoryApp(func(token string) (authn.Identity, error) {
		return authn.ParseAndVerify(token, v.Verify)
	})
	req := httptest.NewRequest(fiber.MethodGet, "/v1/users/me", nil)
	req.Header.Set("Authorization", "Bearer "+signed)
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
	_, jwksURL := signedJWT(t)
	v := authn.NewJWKS(jwksURL)
	app := memoryApp(func(token string) (authn.Identity, error) {
		return authn.ParseAndVerify(token, v.Verify)
	})
	id := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	req := httptest.NewRequest(fiber.MethodGet, "/v1/users/me", nil)
	req.Header.Set("Authorization", "Bearer "+unsignedJWT(
		`{"sub":"`+id.String()+`","email":"yk@example.com","groups":["/UYELER/YK"]}`,
	))
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusUnauthorized {
		t.Fatalf("status %d", resp.StatusCode)
	}
}
