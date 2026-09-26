package httpx_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/testauth"
)

// serviceToken is a client-credentials token of a Keycloak client's service
// account, as Keycloak 26 issues it: azp and client_id name the client.
func serviceToken(t *testing.T, keys *testauth.Bundle, client string, coreRoles ...string) string {
	t.Helper()
	return keys.Token(t, jwt.MapClaims{
		"sub": uuid.NewString(), "azp": client, "client_id": client,
		"preferred_username": "service-account-" + client,
		"resource_access":    map[string]any{"core": map[string]any{"roles": coreRoles}},
	})
}

// personToken is a person's token, signed in through client.
func personToken(t *testing.T, keys *testauth.Bundle, client string, coreRoles ...string) string {
	t.Helper()
	return keys.Token(t, jwt.MapClaims{
		"sub": uuid.NewString(), "azp": client, "email": "editor@example.com", "given_name": "E", "family_name": "D",
		"resource_access": map[string]any{"core": map[string]any{"roles": coreRoles}},
	})
}

type jsonResponse struct {
	status int
	body   map[string]any
}

func sendJSON(t *testing.T, app *fiber.App, token, method, path, body string) jsonResponse {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]any{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatalf("status %d body %s: %v", resp.StatusCode, raw, err)
		}
	}
	return jsonResponse{status: resp.StatusCode, body: got}
}

// uploadAs uploads a PNG for the purpose with the person's token.
func uploadAs(t *testing.T, app *fiber.App, token, purpose string) string {
	t.Helper()
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	if err := form.WriteField("purpose", purpose); err != nil {
		t.Fatal(err)
	}
	part, err := form.CreateFormFile("file", "logo.png")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(pngPicture(t)); err != nil {
		t.Fatal(err)
	}
	if err := form.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(fiber.MethodPost, "/v1/media", &body)
	req.Header.Set("Content-Type", form.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var created map[string]any
	if resp.StatusCode != fiber.StatusCreated || json.Unmarshal(raw, &created) != nil {
		t.Fatalf("upload %s: status %d body %s", purpose, resp.StatusCode, raw)
	}
	return created["id"].(string)
}

func attachBody(service, ownerType, ownerID, role string) string {
	return `{"owner":{"service":"` + service + `","type":"` + ownerType + `","id":"` + ownerID + `"},"role":"` + role + `"}`
}

func TestAProductAttachesMediaToItsOwnRecordHTTP(t *testing.T) {
	t.Parallel()
	keys := testauth.New(t)
	app := memoryApp(keys.Parse())
	mediaID := uploadAs(t, app, personToken(t, keys, "inscribed"), "cms_image")
	page := uuid.NewString()

	resp := sendJSON(t, app, serviceToken(t, keys, "skycms", "media:attach"), fiber.MethodPost,
		"/v1/media/"+mediaID+"/attachments", attachBody("cms", "page", page, "image"))
	if resp.status != fiber.StatusCreated {
		t.Fatalf("attach: status %d body %v", resp.status, resp.body)
	}
	owner, _ := resp.body["owner"].(map[string]any)
	if resp.body["mediaId"] != mediaID || resp.body["role"] != "image" || resp.body["id"] == nil ||
		owner["service"] != "cms" || owner["type"] != "page" || owner["id"] != page {
		t.Fatalf("attachment %v", resp.body)
	}
	got := sendJSON(t, app, personToken(t, keys, "inscribed"), fiber.MethodGet, "/v1/media/"+mediaID, "")
	if got.body["status"] != "attached" || got.body["expiresAt"] != nil {
		t.Fatalf("media after attach: %v", got.body)
	}
}

// requireCode checks a problem+json refusal's status and stable code.
func requireCode(t *testing.T, resp jsonResponse, status int, code string) {
	t.Helper()
	if resp.status != status || resp.body["code"] != code {
		t.Fatalf("status %d body %v; want %d %s", resp.status, resp.body, status, code)
	}
}

// Only a product's service account manages Media attachments. A person is
// refused even with the media:attach role and signed in through a product's
// client, and so is a service account without the role or of a client that
// is no product core knows.
func TestOnlyAProductsServiceAccountWithTheRoleMayAttachHTTP(t *testing.T) {
	t.Parallel()
	keys := testauth.New(t)
	app := memoryApp(keys.Parse())
	mediaID := uploadAs(t, app, personToken(t, keys, "inscribed"), "cms_image")
	body := attachBody("cms", "page", uuid.NewString(), "image")

	for name, token := range map[string]string{
		"person with the role":         personToken(t, keys, "skycms", "media:attach"),
		"service without the role":     serviceToken(t, keys, "skycms", "users:read"),
		"service of an unknown client": serviceToken(t, keys, "frontend-main", "media:attach"),
	} {
		resp := sendJSON(t, app, token, fiber.MethodPost, "/v1/media/"+mediaID+"/attachments", body)
		if resp.status != fiber.StatusForbidden || resp.body["code"] != "media_attach_forbidden" {
			t.Errorf("%s: status %d body %v", name, resp.status, resp.body)
		}
	}
	got := sendJSON(t, app, personToken(t, keys, "inscribed"), fiber.MethodGet, "/v1/media/"+mediaID, "")
	if got.body["status"] != "pending" {
		t.Fatalf("media after refused attaches: %v", got.body)
	}
}

// A product manages only its own Media attachments: Skyforms cannot attach a
// Media to a CMS page, and no product can write core's own links.
func TestAProductCannotAttachForAnotherProductHTTP(t *testing.T) {
	t.Parallel()
	keys := testauth.New(t)
	app := memoryApp(keys.Parse())
	mediaID := uploadAs(t, app, personToken(t, keys, "inscribed"), "cms_image")
	forms := serviceToken(t, keys, "forms", "media:attach")

	for _, service := range []string{"cms", "core"} {
		resp := sendJSON(t, app, forms, fiber.MethodPost, "/v1/media/"+mediaID+"/attachments",
			attachBody(service, "page", uuid.NewString(), "image"))
		requireCode(t, resp, fiber.StatusForbidden, "media_attach_wrong_service")
	}
}

// Attaching the same link again answers the Media attachment already there;
// removing the last Media attachment detaches the Media, which is purged 30
// days later unless something attaches it again. Both calls can be retried.
func TestServiceAttachAndDetachAreIdempotentHTTP(t *testing.T) {
	t.Parallel()
	keys := testauth.New(t)
	app := memoryApp(keys.Parse())
	person := personToken(t, keys, "inscribed")
	mediaID := uploadAs(t, app, person, "cms_image")
	cms := serviceToken(t, keys, "skycms", "media:attach")
	body := attachBody("cms", "page", uuid.NewString(), "image")
	path := "/v1/media/" + mediaID + "/attachments"

	first := sendJSON(t, app, cms, fiber.MethodPost, path, body)
	again := sendJSON(t, app, cms, fiber.MethodPost, path, body)
	if first.status != fiber.StatusCreated || again.status != fiber.StatusOK || again.body["id"] != first.body["id"] {
		t.Fatalf("attach twice: %d %v, then %d %v", first.status, first.body, again.status, again.body)
	}

	before := time.Now()
	for range 2 {
		resp := sendJSON(t, app, cms, fiber.MethodDelete, path+"/"+first.body["id"].(string), "")
		if resp.status != fiber.StatusNoContent {
			t.Fatalf("detach: status %d body %v", resp.status, resp.body)
		}
	}
	after := time.Now()
	got := sendJSON(t, app, person, fiber.MethodGet, "/v1/media/"+mediaID, "")
	expires, err := time.Parse(time.RFC3339Nano, fmt.Sprint(got.body["expiresAt"]))
	window := 30 * 24 * time.Hour
	if got.body["status"] != "detached" || err != nil || expires.Before(before.Add(window)) || expires.After(after.Add(window)) {
		t.Fatalf("media after detach: %v", got.body)
	}
}
