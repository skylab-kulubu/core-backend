package httpx_test

import (
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/testauth"
)

// An ID token of the same realm, even one whose audience names core, is not
// a Bearer: a route answers it 401 as it does a bad signature. A person's
// access token and a service account's (client credentials: no e-mail, a
// service-account- username) both pass.
func TestBearerRouteRefusesAnIDToken(t *testing.T) {
	t.Parallel()
	keys := testauth.New(t)
	app := memoryApp(keys.Parse())
	person := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	service := uuid.MustParse("33333333-3333-3333-3333-333333333333")

	send := func(token string) int {
		t.Helper()
		req := httptest.NewRequest(fiber.MethodGet, "/v1/users/me", nil)
		req.Header.Set(fiber.HeaderAuthorization, "Bearer "+token)
		resp, err := app.Test(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode
	}

	access := keys.Token(t, jwt.MapClaims{"sub": person.String(), "email": "ada@example.com", "typ": "Bearer", "azp": "frontend-main"})
	if got := send(access); got == fiber.StatusUnauthorized {
		t.Fatalf("access token: %d", got)
	}
	serviceAccount := keys.Token(t, jwt.MapClaims{
		"sub": service.String(), "typ": "Bearer", "azp": "forms", "preferred_username": "service-account-forms",
	})
	if got := send(serviceAccount); got == fiber.StatusUnauthorized {
		t.Fatalf("service account token: %d", got)
	}
	for _, typ := range []string{"ID", "Refresh"} {
		other := keys.Token(t, jwt.MapClaims{"sub": person.String(), "email": "ada@example.com", "typ": typ, "azp": "core"})
		if got := send(other); got != fiber.StatusUnauthorized {
			t.Fatalf("%s token: %d", typ, got)
		}
	}
}
