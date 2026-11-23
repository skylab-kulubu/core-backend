package handlers

import (
	"context"
	"encoding/json"
	"io"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authn"
	"github.com/skylab-kulubu/core-backend/internal/identity"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

func TestUserDirectoryHTTP(t *testing.T) {
	t.Parallel()
	store := user.NewMemoryStore()
	id := uuid.MustParse("33333333-3333-3333-3333-333333333333")
	if _, _, err := store.Upsert(context.Background(), user.User{
		ID: id, Email: "ada@example.test", FirstName: "Ada", LastName: "Lovelace",
		SchoolEmail: "ada@std.yildiz.edu.tr", SkyNumber: "SKY-0001",
	}); err != nil {
		t.Fatal(err)
	}
	reader := authn.Identity{ID: uuid.MustParse("44444444-4444-4444-4444-444444444444"), Roles: []string{"users:read"}}
	member := authn.Identity{ID: uuid.MustParse("22222222-2222-2222-2222-222222222222"), Groups: []string{"/UYELER/ARGE/WEBLAB"}}

	get := func(ident authn.Identity, target string) (int, map[string]any) {
		t.Helper()
		resp, err := identityApp(t, ident, identity.NewMemory(), store).Test(httptest.NewRequest(fiber.MethodGet, target, nil))
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		var out map[string]any
		_ = json.Unmarshal(body, &out)
		return resp.StatusCode, out
	}

	status, body := get(ykIdent(), "/v1/users/directory?page=1&size=10&q=ada")
	if status != fiber.StatusOK || body["total"] != float64(1) || body["page"] != float64(1) || body["size"] != float64(10) {
		t.Fatalf("yk: %d %v", status, body)
	}
	item := body["items"].([]any)[0].(map[string]any)
	if item["id"] != id.String() || item["skyNumber"] != "SKY-0001" || item["schoolEmail"] != "ada@std.yildiz.edu.tr" || item["status"] != "active" || item["createdAt"] == nil {
		t.Fatalf("yk item: %v", item)
	}

	status, body = get(reader, "/v1/users/directory")
	if status != fiber.StatusOK || body["size"] != float64(50) {
		t.Fatalf("reader: %d %v", status, body)
	}
	item = body["items"].([]any)[0].(map[string]any)
	for _, hidden := range []string{"skyNumber", "schoolEmail", "username", "createdAt"} {
		if _, ok := item[hidden]; ok {
			t.Fatalf("reader item carries %s: %v", hidden, item)
		}
	}
	if status, _ := get(reader, "/v1/users/directory?filter=no_school_email"); status != fiber.StatusForbidden {
		t.Fatalf("reader filter: %d", status)
	}

	for target, want := range map[string]int{
		"/v1/users/directory":          fiber.StatusForbidden,
		"/v1/users/directory?page=abc": fiber.StatusForbidden,
	} {
		if status, _ := get(member, target); status != want {
			t.Fatalf("member %s: %d, want %d", target, status, want)
		}
	}
	for _, target := range []string{
		"/v1/users/directory?page=0",
		"/v1/users/directory?page=abc",
		"/v1/users/directory?size=-1",
		"/v1/users/directory?filter=all",
	} {
		if status, _ := get(ykIdent(), target); status != fiber.StatusBadRequest {
			t.Fatalf("yk %s: %d, want 400", target, status)
		}
	}
	// A page past the last one: 400 page_invalid, never an overflowed offset
	status, body = get(ykIdent(), "/v1/users/directory?page=200000000000000000")
	if status != fiber.StatusBadRequest || body["code"] != "page_invalid" {
		t.Fatalf("huge page: %d %v", status, body)
	}
	if status, _ := get(member, "/v1/users/directory?page=200000000000000000"); status != fiber.StatusForbidden {
		t.Fatalf("member huge page: %d", status)
	}
	if status, _ := get(authn.Identity{}, "/v1/users/directory"); status != fiber.StatusUnauthorized {
		t.Fatalf("anonymous: %d", status)
	}
}
