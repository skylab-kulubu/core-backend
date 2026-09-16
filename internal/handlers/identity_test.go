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
	"github.com/skylab-kulubu/core-backend/internal/identity"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

func identityApp(t *testing.T, ident authn.Identity, dir *identity.Memory, store *user.MemoryStore) *fiber.App {
	t.Helper()
	svc := identity.NewService(dir, store, authz.NewAuthorizer(authz.DefaultPolicy()))
	h := NewIdentityHandler(svc)
	app := fiber.New()
	app.Use(func(c fiber.Ctx) error {
		if ident.ID != uuid.Nil || len(ident.Groups) > 0 {
			c.Locals(authn.LocalsIdentity, ident)
		}
		return c.Next()
	})
	app.Get("/v1/groups", h.ListGroups)
	app.Get("/v1/groups/:groupId/members", h.Members)
	app.Post("/v1/groups/:groupId/members", h.AddMember)
	app.Delete("/v1/groups/:groupId/members/:userId", h.RemoveMember)
	app.Post("/v1/users", h.CreateUser)
	app.Delete("/v1/users/:id", h.DeleteUser)
	return app
}

func ykIdent() authn.Identity {
	return authn.Identity{
		ID:     uuid.MustParse("11111111-1111-1111-1111-111111111111"),
		Groups: []string{"/UYELER/YK"},
	}
}

func TestListGroupsUnauthorized(t *testing.T) {
	t.Parallel()
	app := identityApp(t, authn.Identity{}, identity.NewMemory(), user.NewMemoryStore())
	resp, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/v1/groups", nil))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusUnauthorized {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

func TestListGroupsForbiddenForMember(t *testing.T) {
	t.Parallel()
	ident := authn.Identity{
		ID:     uuid.MustParse("22222222-2222-2222-2222-222222222222"),
		Groups: []string{"/UYELER/ARGE/WEBLAB"},
	}
	app := identityApp(t, ident, identity.NewMemory(), user.NewMemoryStore())
	resp, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/v1/groups", nil))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusForbidden {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d body %s", resp.StatusCode, body)
	}
}

func TestPromoteAndRosterHTTP(t *testing.T) {
	t.Parallel()
	dir := identity.NewMemory()
	store := user.NewMemoryStore()
	dir.PutGroup(identity.Group{ID: "g-uyeler", Name: "UYELER", Path: "/UYELER"})
	ghostID := uuid.MustParse("eeeeeeee-eeee-eeee-eeee-eeeeeeeeeeee")
	dir.PutUser(identity.Person{ID: ghostID, Email: "ghost@example.com", FirstName: "Ghost"})

	app := identityApp(t, ykIdent(), dir, store)
	body := `{"userId":"` + ghostID.String() + `"}`
	req := httptest.NewRequest(fiber.MethodPost, "/v1/groups/g-uyeler/members", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusNoContent {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("promote status %d body %s", resp.StatusCode, b)
	}

	resp, err = app.Test(httptest.NewRequest(fiber.MethodGet, "/v1/groups/g-uyeler/members", nil))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("roster status %d", resp.StatusCode)
	}
	var members []identity.Person
	if err := json.NewDecoder(resp.Body).Decode(&members); err != nil {
		t.Fatal(err)
	}
	if len(members) != 1 || members[0].ID != ghostID {
		t.Fatalf("members %+v", members)
	}
}

func TestCreateAndDeleteUserHTTP(t *testing.T) {
	t.Parallel()
	dir := identity.NewMemory()
	store := user.NewMemoryStore()
	app := identityApp(t, ykIdent(), dir, store)

	req := httptest.NewRequest(fiber.MethodPost, "/v1/users", strings.NewReader(
		`{"email":"ada@example.com","firstName":"Ada","lastName":"Lovelace"}`,
	))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("create status %d body %s", resp.StatusCode, b)
	}
	var created identity.Person
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}

	del := httptest.NewRequest(fiber.MethodDelete, "/v1/users/"+created.ID.String(), nil)
	resp, err = app.Test(del)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusNoContent {
		t.Fatalf("delete status %d", resp.StatusCode)
	}
}
