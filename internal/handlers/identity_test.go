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
	return identityAppWithErasure(t, ident, dir, store, true)
}

func identityAppWithErasure(t *testing.T, ident authn.Identity, dir *identity.Memory, store *user.MemoryStore, enabled bool) *fiber.App {
	t.Helper()
	svc := identity.NewServiceWithOptions(dir, store, authz.NewAuthorizer(authz.DefaultPolicy()), identity.Options{
		AccountErasureEnabled: enabled,
	})
	h := NewIdentityHandler(svc)
	app := fiber.New()
	app.Use(func(c fiber.Ctx) error {
		if ident.ID != uuid.Nil || len(ident.Groups) > 0 {
			c.Locals(authn.LocalsIdentity, ident)
		}
		return c.Next()
	})
	app.Get("/v1/groups", h.ListGroups)
	app.Post("/v1/groups", h.CreateGroup)
	app.Get("/v1/groups/:groupId", h.GetGroup)
	app.Patch("/v1/groups/:groupId", h.UpdateGroup)
	app.Get("/v1/groups/:groupId/members", h.Members)
	app.Post("/v1/groups/:groupId/members", h.AddMember)
	app.Delete("/v1/groups/:groupId/members/:userId", h.RemoveMember)
	app.Get("/v1/groups/:groupId/client-roles", h.GroupClientRoles)
	app.Put("/v1/groups/:groupId/client-roles", h.SetGroupClientRoles)
	app.Get("/v1/users", h.ListUsers)
	app.Get("/v1/users/:id", h.GetUser)
	app.Patch("/v1/users/:id", h.PatchUser)
	app.Post("/v1/users", h.CreateUser)
	app.Delete("/v1/users/:id", h.DeleteUser)
	app.Post("/v1/users/:id/logout", h.LogoutAllSessions)
	app.Post("/v1/users/:id/client-roles", h.AddUserExtraRole)
	app.Delete("/v1/users/:id/client-roles", h.RemoveUserExtraRole)
	app.Get("/v1/client-roles", h.ListClientRoles)
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

func TestGroupMembersHTTPIncludesSubgroups(t *testing.T) {
	t.Parallel()
	dir := identity.NewMemory()
	dir.PutGroup(identity.Group{ID: "g-yk", Name: "YK", Path: "/UYELER/YK"})
	dir.PutGroup(identity.Group{ID: "g-baskan", Name: "BASKAN", Path: "/UYELER/YK/BASKAN"})
	id := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaa21")
	dir.PutUser(identity.Person{ID: id, Email: "yk@example.com", FirstName: "Yk"})
	if err := dir.AddMember(t.Context(), "g-baskan", id); err != nil {
		t.Fatal(err)
	}
	app := identityApp(t, ykIdent(), dir, user.NewMemoryStore())
	resp, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/v1/groups/g-yk/members", nil))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("roster status %d", resp.StatusCode)
	}
	var members []identity.GroupMember
	if err := json.NewDecoder(resp.Body).Decode(&members); err != nil {
		t.Fatal(err)
	}
	if len(members) != 1 || members[0].ID != id {
		t.Fatalf("members %+v", members)
	}
	if members[0].SourceGroupPath != "/UYELER/YK/BASKAN" || members[0].SourceGroupID != "g-baskan" {
		t.Fatalf("source %+v", members[0])
	}

	del := httptest.NewRequest(fiber.MethodDelete, "/v1/groups/g-yk/members/"+id.String(), nil)
	resp, err = app.Test(del)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusNoContent {
		t.Fatalf("remove status %d", resp.StatusCode)
	}
	resp, err = app.Test(httptest.NewRequest(fiber.MethodGet, "/v1/groups/g-yk/members", nil))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.NewDecoder(resp.Body).Decode(&members); err != nil {
		t.Fatal(err)
	}
	if len(members) != 0 {
		t.Fatalf("nested member still on parent after remove %+v", members)
	}
}

func TestCreateAndDeleteUserHTTP(t *testing.T) {
	t.Parallel()
	dir := identity.NewMemory()
	store := user.NewMemoryStore()
	if _, _, err := user.NewService(store).Ensure(t.Context(), ykIdent().ID, user.Profile{Email: "yk-operator@example.test"}); err != nil {
		t.Fatal(err)
	}
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

func TestDeleteUserHTTPIsUnavailableWhileErasureReleaseGateIsOff(t *testing.T) {
	t.Parallel()
	dir := identity.NewMemory()
	store := user.NewMemoryStore()
	targetID := uuid.New()
	if _, _, err := user.NewService(store).Ensure(t.Context(), targetID, user.Profile{Email: "held@example.test"}); err != nil {
		t.Fatal(err)
	}
	dir.PutUser(identity.Person{ID: targetID, Email: "held@example.test"})
	app := identityAppWithErasure(t, ykIdent(), dir, store, false)

	resp, err := app.Test(httptest.NewRequest(fiber.MethodDelete, "/v1/users/"+targetID.String(), nil))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusServiceUnavailable {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	got, err := store.Get(t.Context(), targetID)
	if err != nil || got.AccountState != user.AccountActive {
		t.Fatalf("account state=%q err=%v", got.AccountState, err)
	}
}

func TestUserCardAndGroupRolesHTTP(t *testing.T) {
	t.Parallel()
	dir := identity.NewMemory()
	store := user.NewMemoryStore()
	dir.PutGroup(identity.Group{ID: "g-weblab", Name: "WEBLAB", Path: "/UYELER/ARGE/WEBLAB"})
	id := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	dir.PutUser(identity.Person{ID: id, Email: "ada@example.com", FirstName: "Ada"})
	if err := dir.AddMember(t.Context(), "g-weblab", id); err != nil {
		t.Fatal(err)
	}
	app := identityApp(t, ykIdent(), dir, store)

	body := `[{"clientId":"skyforms","role":"skyforms:access"}]`
	req := httptest.NewRequest(fiber.MethodPut, "/v1/groups/g-weblab/client-roles", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusNoContent {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("map status %d body %s", resp.StatusCode, b)
	}

	extra := `{"clientId":"skyforms","role":"skyforms:form:manage"}`
	req = httptest.NewRequest(fiber.MethodPost, "/v1/users/"+id.String()+"/client-roles", strings.NewReader(extra))
	req.Header.Set("Content-Type", "application/json")
	resp, err = app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusNoContent {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("extra status %d body %s", resp.StatusCode, b)
	}

	resp, err = app.Test(httptest.NewRequest(fiber.MethodGet, "/v1/users/"+id.String(), nil))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("get status %d", resp.StatusCode)
	}
	var card identity.UserCard
	if err := json.NewDecoder(resp.Body).Decode(&card); err != nil {
		t.Fatal(err)
	}
	if len(card.InheritedRoles) != 1 || len(card.ExtraRoles) != 1 {
		t.Fatalf("card %+v", card)
	}

	resp, err = app.Test(httptest.NewRequest(fiber.MethodGet, "/v1/groups/g-weblab", nil))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("group status %d", resp.StatusCode)
	}
}

func TestCreateGroupAndLogout(t *testing.T) {
	t.Parallel()
	dir := identity.NewMemory()
	dir.PutGroup(identity.Group{ID: "g-arge", Name: "ARGE", Path: "/UYELER/ARGE"})
	id := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	dir.PutUser(identity.Person{ID: id, Email: "ada@example.com"})
	app := identityApp(t, ykIdent(), dir, user.NewMemoryStore())

	req := httptest.NewRequest(fiber.MethodPost, "/v1/groups", strings.NewReader(`{"name":"WEBLAB","parentId":"g-arge"}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d body %s", resp.StatusCode, body)
	}
	var created identity.Group
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	if created.Path != "/UYELER/ARGE/WEBLAB" {
		t.Fatalf("created %+v", created)
	}

	req = httptest.NewRequest(fiber.MethodPatch, "/v1/groups/"+created.ID, strings.NewReader(`{"name":"WEBLAB-X"}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err = app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("rename status %d body %s", resp.StatusCode, body)
	}

	req = httptest.NewRequest(fiber.MethodPost, "/v1/users/"+id.String()+"/logout", nil)
	resp, err = app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusNoContent {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("logout status %d body %s", resp.StatusCode, body)
	}
}

func TestListUsersSearchHTTP(t *testing.T) {
	t.Parallel()
	dir := identity.NewMemory()
	store := user.NewMemoryStore()
	ada := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	dir.PutUser(identity.Person{ID: ada, Email: "ada@example.com", FirstName: "Ada"})
	if _, _, err := store.Upsert(t.Context(), user.User{
		ID: ada, Email: "ada@example.com", FirstName: "Ada", LastName: "Lovelace", SchoolEmail: "ada@std.yildiz.edu.tr",
	}); err != nil {
		t.Fatal(err)
	}
	app := identityApp(t, ykIdent(), dir, store)

	resp, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/v1/users?q=std.yildiz", nil))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var found []identity.Person
	if err := json.NewDecoder(resp.Body).Decode(&found); err != nil {
		t.Fatal(err)
	}
	if len(found) != 1 || found[0].SchoolEmail != "ada@std.yildiz.edu.tr" {
		t.Fatalf("found %+v", found)
	}
}

func TestPatchOtherUserHTTP(t *testing.T) {
	t.Parallel()
	dir := identity.NewMemory()
	store := user.NewMemoryStore()
	id := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	dir.PutUser(identity.Person{ID: id, Email: "ada@example.com", FirstName: "Ada", LastName: "Lovelace"})
	if _, _, err := store.Upsert(t.Context(), user.User{
		ID: id, Email: "ada@example.com", FirstName: "Ada", LastName: "Lovelace", SkyNumber: "SKY-0000001",
	}); err != nil {
		t.Fatal(err)
	}
	app := identityApp(t, ykIdent(), dir, store)

	req := httptest.NewRequest(fiber.MethodPatch, "/v1/users/"+id.String(), strings.NewReader(
		`{"firstName":"Ada","lastName":"Byron","linkedin":"https://linkedin.com/in/ada","university":"YTÜ","faculty":"EE","department":"CE","phone":"+905551112233"}`,
	))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("patch status %d body %s", resp.StatusCode, b)
	}
	var patched map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&patched); err != nil {
		t.Fatal(err)
	}
	if patched["lastName"] != "Byron" || patched["phone"] != "+905551112233" || patched["university"] != "YTÜ" || patched["skyNumber"] != "SKY-0000001" {
		t.Fatalf("patched %+v", patched)
	}

	resp, err = app.Test(httptest.NewRequest(fiber.MethodGet, "/v1/users/"+id.String(), nil))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("get status %d", resp.StatusCode)
	}
	var got map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got["phone"] != "+905551112233" || got["department"] != "CE" || got["linkedin"] != "https://linkedin.com/in/ada" {
		t.Fatalf("get %+v", got)
	}
}

func TestPatchOtherUserForbiddenHTTP(t *testing.T) {
	t.Parallel()
	dir := identity.NewMemory()
	store := user.NewMemoryStore()
	id := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	dir.PutUser(identity.Person{ID: id, Email: "ada@example.com", FirstName: "Ada"})
	ident := authn.Identity{
		ID:     uuid.MustParse("22222222-2222-2222-2222-222222222222"),
		Groups: []string{"/UYELER/ARGE/WEBLAB"},
	}
	app := identityApp(t, ident, dir, store)
	req := httptest.NewRequest(fiber.MethodPatch, "/v1/users/"+id.String(), strings.NewReader(`{"phone":"+905551112233"}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusForbidden {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d body %s", resp.StatusCode, b)
	}
}

func TestListUsersSeatPinsFormsIgnoringClientIdQuery(t *testing.T) {
	t.Parallel()
	dir := identity.NewMemory()
	store := user.NewMemoryStore()
	dir.PutGroup(identity.Group{ID: "g-weblab", Name: "WEBLAB", Path: "/UYELER/ARGE/WEBLAB"})
	seated := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	other := uuid.MustParse("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")
	dir.PutUser(identity.Person{ID: seated, Email: "ada@example.com", FirstName: "Ada"})
	dir.PutUser(identity.Person{ID: other, Email: "other@example.com", FirstName: "Other"})
	if err := dir.AddMember(t.Context(), "g-weblab", seated); err != nil {
		t.Fatal(err)
	}
	if err := dir.SetGroupClientRoles(t.Context(), "g-weblab", []identity.ClientRole{{ClientID: "forms", Role: "skyforms:access"}}); err != nil {
		t.Fatal(err)
	}
	app := identityApp(t, ykIdent(), dir, store)

	resp, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/v1/users?role=skyforms:access&clientId=core", nil))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d body %s", resp.StatusCode, body)
	}
	var found []identity.Person
	if err := json.NewDecoder(resp.Body).Decode(&found); err != nil {
		t.Fatal(err)
	}
	if len(found) != 1 || found[0].ID != seated {
		t.Fatalf("found %+v", found)
	}
}

func TestListClientRolesHTTP(t *testing.T) {
	t.Parallel()
	dir := identity.NewMemory()
	dir.PutClientRole(identity.ClientRole{ClientID: "skyforms", Role: "skyforms:access"})
	app := identityApp(t, ykIdent(), dir, user.NewMemoryStore())
	resp, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/v1/client-roles", nil))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d body %s", resp.StatusCode, body)
	}
	var roles []identity.ClientRole
	if err := json.NewDecoder(resp.Body).Decode(&roles); err != nil {
		t.Fatal(err)
	}
	if len(roles) != 1 || roles[0].Role != "skyforms:access" {
		t.Fatalf("roles %+v", roles)
	}
}
