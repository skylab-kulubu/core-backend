package handlers

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
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

const adminOnlyPhone = "+905559876543"

func memberIdent() authn.Identity {
	return authn.Identity{
		ID:     uuid.MustParse("22222222-2222-2222-2222-222222222222"),
		Groups: []string{"/UYELER/ARGE/WEBLAB"},
	}
}

func seedShadowUser(t *testing.T, dir *identity.Memory, store *user.MemoryStore, id uuid.UUID) {
	t.Helper()
	dir.PutUser(identity.Person{ID: id, Email: "ada@example.com", FirstName: "Ada", LastName: "Lovelace"})
	if _, _, err := store.Upsert(t.Context(), user.User{
		ID: id, Email: "ada@example.com", FirstName: "Ada", LastName: "Lovelace", SkyNumber: "SKY-0000001",
		Linkedin: "https://linkedin.com/in/ada", University: "YTÜ", Faculty: "EE", Department: "CE",
	}); err != nil {
		t.Fatal(err)
	}
}

func patchUserJSON(t *testing.T, app *fiber.App, id uuid.UUID, body string) *http.Response {
	t.Helper()
	req := httptest.NewRequest(fiber.MethodPatch, "/v1/users/"+id.String(), strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestMemberCannotPatchOtherUserHTTP(t *testing.T) {
	t.Parallel()
	dir := identity.NewMemory()
	store := user.NewMemoryStore()
	id := uuid.MustParse("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbb1")
	seedShadowUser(t, dir, store, id)

	app := identityApp(t, memberIdent(), dir, store)
	resp := patchUserJSON(t, app, id, `{"lastName":"Hacker","phone":"`+adminOnlyPhone+`"}`)
	if resp.StatusCode != fiber.StatusForbidden {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d body %s", resp.StatusCode, b)
	}

	admin := identityApp(t, ykIdent(), dir, store)
	got, err := admin.Test(httptest.NewRequest(fiber.MethodGet, "/v1/users/"+id.String(), nil))
	if err != nil {
		t.Fatal(err)
	}
	if got.StatusCode != fiber.StatusOK {
		t.Fatalf("get status %d", got.StatusCode)
	}
	raw, err := io.ReadAll(got.Body)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), adminOnlyPhone) || strings.Contains(string(raw), "Hacker") {
		t.Fatalf("member write leaked: %s", raw)
	}
}

func TestPatchOtherUserOmitLeavesEmptyClearsHTTP(t *testing.T) {
	t.Parallel()
	dir := identity.NewMemory()
	store := user.NewMemoryStore()
	id := uuid.MustParse("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbb2")
	seedShadowUser(t, dir, store, id)
	app := identityApp(t, ykIdent(), dir, store)

	resp := patchUserJSON(t, app, id, `{"phone":"`+adminOnlyPhone+`"}`)
	if resp.StatusCode != fiber.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("set phone status %d body %s", resp.StatusCode, b)
	}

	resp = patchUserJSON(t, app, id, `{"department":"CS"}`)
	if resp.StatusCode != fiber.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("omit status %d body %s", resp.StatusCode, b)
	}
	var omitted map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&omitted); err != nil {
		t.Fatal(err)
	}
	if omitted["phone"] != adminOnlyPhone || omitted["university"] != "YTÜ" || omitted["department"] != "CS" || omitted["skyNumber"] != "SKY-0000001" {
		t.Fatalf("omit %+v", omitted)
	}

	resp = patchUserJSON(t, app, id, `{"firstName":"","phone":"","linkedin":""}`)
	if resp.StatusCode != fiber.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("clear status %d body %s", resp.StatusCode, b)
	}
	var cleared map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&cleared); err != nil {
		t.Fatal(err)
	}
	if phone, ok := cleared["phone"]; ok && phone != "" && phone != nil {
		t.Fatalf("phone not cleared %+v", cleared)
	}
	if cleared["linkedin"] != "" && cleared["linkedin"] != nil {
		t.Fatalf("linkedin not cleared %+v", cleared)
	}
	if cleared["firstName"] != "" {
		t.Fatalf("first name not cleared %+v", cleared)
	}
	if cleared["department"] != "CS" || cleared["university"] != "YTÜ" {
		t.Fatalf("cleared wiped neighbors %+v", cleared)
	}
}

func TestGetMeOmitsPhoneAfterAdminPatchHTTP(t *testing.T) {
	t.Parallel()
	dir := identity.NewMemory()
	store := user.NewMemoryStore()
	id := uuid.MustParse("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbb3")
	seedShadowUser(t, dir, store, id)
	admin := identityApp(t, ykIdent(), dir, store)

	resp := patchUserJSON(t, admin, id, `{"phone":"`+adminOnlyPhone+`"}`)
	if resp.StatusCode != fiber.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("patch status %d body %s", resp.StatusCode, b)
	}

	meApp := meIdentApp(t, store, id, user.Profile{Email: "ada@example.com", FirstName: "Ada", LastName: "Lovelace"}, nil)
	meResp, err := meApp.Test(httptest.NewRequest(fiber.MethodGet, "/v1/users/me", nil))
	if err != nil {
		t.Fatal(err)
	}
	if meResp.StatusCode != fiber.StatusOK {
		b, _ := io.ReadAll(meResp.Body)
		t.Fatalf("me status %d body %s", meResp.StatusCode, b)
	}
	raw, err := io.ReadAll(meResp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), adminOnlyPhone) {
		t.Fatalf("phone leaked on /me: %s", raw)
	}
	var me map[string]any
	if err := json.Unmarshal(raw, &me); err != nil {
		t.Fatal(err)
	}
	if _, ok := me["phone"]; ok {
		t.Fatalf("phone key on /me: %s", raw)
	}

	cardResp, err := admin.Test(httptest.NewRequest(fiber.MethodGet, "/v1/users/"+id.String(), nil))
	if err != nil {
		t.Fatal(err)
	}
	if cardResp.StatusCode != fiber.StatusOK {
		t.Fatalf("card status %d", cardResp.StatusCode)
	}
	var card map[string]any
	if err := json.NewDecoder(cardResp.Body).Decode(&card); err != nil {
		t.Fatal(err)
	}
	if card["phone"] != adminOnlyPhone {
		t.Fatalf("admin card lost phone %+v", card)
	}
}

func TestPublicRosterOmitsPhoneAfterAdminPatchHTTP(t *testing.T) {
	t.Parallel()
	dir := identity.NewMemory()
	store := user.NewMemoryStore()
	id := uuid.MustParse("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbb4")
	seedShadowUser(t, dir, store, id)
	dir.PutGroup(identity.Group{
		ID:         "g-weblab",
		Name:       "WEBLAB",
		Path:       "/UYELER/ARGE/WEBLAB",
		Attributes: map[string]string{"public_listing": "true"},
	})
	if err := dir.AddMember(t.Context(), "g-weblab", id); err != nil {
		t.Fatal(err)
	}

	admin := identityApp(t, ykIdent(), dir, store)
	resp := patchUserJSON(t, admin, id, `{"phone":"`+adminOnlyPhone+`"}`)
	if resp.StatusCode != fiber.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("patch status %d body %s", resp.StatusCode, b)
	}

	svc := identity.NewService(dir, store, authz.NewAuthorizer(authz.DefaultPolicy()))
	public := fiber.New()
	public.Get("/v1/teams/:team/members", NewTeamHandler(svc).Members)
	rosterResp, err := public.Test(httptest.NewRequest(fiber.MethodGet, "/v1/teams/WEBLAB/members", nil))
	if err != nil {
		t.Fatal(err)
	}
	if rosterResp.StatusCode != fiber.StatusOK {
		b, _ := io.ReadAll(rosterResp.Body)
		t.Fatalf("roster status %d body %s", rosterResp.StatusCode, b)
	}
	raw, err := io.ReadAll(rosterResp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), adminOnlyPhone) || strings.Contains(string(raw), `"phone"`) {
		t.Fatalf("phone leaked on roster: %s", raw)
	}

	cardResp, err := admin.Test(httptest.NewRequest(fiber.MethodGet, "/v1/users/"+id.String(), nil))
	if err != nil {
		t.Fatal(err)
	}
	var card map[string]any
	if err := json.NewDecoder(cardResp.Body).Decode(&card); err != nil {
		t.Fatal(err)
	}
	if card["phone"] != adminOnlyPhone {
		t.Fatalf("admin card lost phone %+v", card)
	}
}

func usersReadIdent() authn.Identity {
	return authn.Identity{
		ID:    uuid.MustParse("33333333-3333-3333-3333-333333333333"),
		Roles: []string{"users:read"},
	}
}

func TestUsersReadListUsersOmitsPrivateDirectoryFieldsHTTP(t *testing.T) {
	t.Parallel()
	dir := identity.NewMemory()
	store := user.NewMemoryStore()
	id := uuid.MustParse("cccccccc-cccc-cccc-cccc-cccccccccccc")
	dir.PutUser(identity.Person{
		ID: id, Email: "ada@example.com", FirstName: "Ada", LastName: "Lovelace",
		Username: "private-user", SchoolEmail: "ada@std.yildiz.edu.tr", SkyNumber: "SKY-0000001",
	})
	dir.PutGroup(identity.Group{ID: "g-forms", Name: "FORMS", Path: "/SERVICES/FORMS"})
	if err := dir.AddMember(t.Context(), "g-forms", id); err != nil {
		t.Fatal(err)
	}
	if err := dir.SetGroupClientRoles(t.Context(), "g-forms", []identity.ClientRole{{ClientID: "forms", Role: "skyforms:access"}}); err != nil {
		t.Fatal(err)
	}
	app := identityApp(t, usersReadIdent(), dir, store)

	resp, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/v1/users", nil))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d body %s", resp.StatusCode, body)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	for _, private := range []string{"username", "schoolEmail", "skyNumber", "private-user", "std.yildiz", "SKY-0000001"} {
		if strings.Contains(string(raw), private) {
			t.Fatalf("private directory field leaked in list: %s", raw)
		}
	}
	if _, err := store.Get(t.Context(), id); !errors.Is(err, user.ErrNotFound) {
		t.Fatalf("users:read list mutated the shadow store: %v", err)
	}

	resp, err = app.Test(httptest.NewRequest(fiber.MethodGet, "/v1/users?q=std.yildiz", nil))
	if err != nil {
		t.Fatal(err)
	}
	var privateMatches []identity.Person
	if err := json.NewDecoder(resp.Body).Decode(&privateMatches); err != nil {
		t.Fatal(err)
	}
	if len(privateMatches) != 0 {
		t.Fatalf("users:read could search a private field: %+v", privateMatches)
	}

	resp, err = app.Test(httptest.NewRequest(fiber.MethodGet, "/v1/users?role=skyforms:access&q=std.yildiz", nil))
	if err != nil {
		t.Fatal(err)
	}
	privateMatches = nil
	if err := json.NewDecoder(resp.Body).Decode(&privateMatches); err != nil {
		t.Fatal(err)
	}
	if len(privateMatches) != 0 {
		t.Fatalf("users:read seat search exposed a private-field match: %+v", privateMatches)
	}
}

func TestUsersReadGetUserOmitsPhoneHTTP(t *testing.T) {
	t.Parallel()
	dir := identity.NewMemory()
	store := user.NewMemoryStore()
	id := uuid.MustParse("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbb5")
	seedShadowUser(t, dir, store, id)

	admin := identityApp(t, ykIdent(), dir, store)
	resp := patchUserJSON(t, admin, id, `{"phone":"`+adminOnlyPhone+`"}`)
	if resp.StatusCode != fiber.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("patch status %d body %s", resp.StatusCode, b)
	}

	reader := identityApp(t, usersReadIdent(), dir, store)
	got, err := reader.Test(httptest.NewRequest(fiber.MethodGet, "/v1/users/"+id.String(), nil))
	if err != nil {
		t.Fatal(err)
	}
	if got.StatusCode != fiber.StatusOK {
		b, _ := io.ReadAll(got.Body)
		t.Fatalf("users:read status %d body %s", got.StatusCode, b)
	}
	raw, err := io.ReadAll(got.Body)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), adminOnlyPhone) {
		t.Fatalf("phone leaked to users:read: %s", raw)
	}
	var card map[string]any
	if err := json.Unmarshal(raw, &card); err != nil {
		t.Fatal(err)
	}
	if _, ok := card["phone"]; ok {
		t.Fatalf("phone key on users:read card: %s", raw)
	}
	if card["id"] != id.String() || card["firstName"] != "Ada" || card["lastName"] != "Lovelace" || card["email"] != "ada@example.com" {
		t.Fatalf("users:read lost name card %+v", card)
	}

	adminGot, err := admin.Test(httptest.NewRequest(fiber.MethodGet, "/v1/users/"+id.String(), nil))
	if err != nil {
		t.Fatal(err)
	}
	if adminGot.StatusCode != fiber.StatusOK {
		t.Fatalf("admin get status %d", adminGot.StatusCode)
	}
	var adminCard map[string]any
	if err := json.NewDecoder(adminGot.Body).Decode(&adminCard); err != nil {
		t.Fatal(err)
	}
	if adminCard["phone"] != adminOnlyPhone {
		t.Fatalf("admin card lost phone %+v", adminCard)
	}
}

func TestPrivilegedGetUserIncludesStudentCardUidHTTP(t *testing.T) {
	t.Parallel()
	dir := identity.NewMemory()
	store := user.NewMemoryStore()
	id := uuid.MustParse("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbb6")
	seedShadowUser(t, dir, store, id)
	uid := "AABBCCDDEEFF"
	existing, err := store.Get(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	existing.StudentCardUID = uid
	if _, err := store.UpdateProfile(t.Context(), existing); err != nil {
		t.Fatal(err)
	}

	admin := identityApp(t, ykIdent(), dir, store)
	resp, err := admin.Test(httptest.NewRequest(fiber.MethodGet, "/v1/users/"+id.String(), nil))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("privileged get %d body %s", resp.StatusCode, b)
	}
	var card map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&card); err != nil {
		t.Fatal(err)
	}
	if card["studentCardUid"] != uid {
		t.Fatalf("privileged card missing uid %+v", card)
	}

	reader := identityApp(t, usersReadIdent(), dir, store)
	got, err := reader.Test(httptest.NewRequest(fiber.MethodGet, "/v1/users/"+id.String(), nil))
	if err != nil {
		t.Fatal(err)
	}
	if got.StatusCode != fiber.StatusOK {
		t.Fatalf("users:read status %d", got.StatusCode)
	}
	raw, err := io.ReadAll(got.Body)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), uid) || strings.Contains(string(raw), "studentCardUid") {
		t.Fatalf("uid leaked to users:read: %s", raw)
	}

	meApp := meIdentApp(t, store, id, user.Profile{Email: "ada@example.com", FirstName: "Ada", LastName: "Lovelace"}, nil)
	meResp, err := meApp.Test(httptest.NewRequest(fiber.MethodGet, "/v1/users/me", nil))
	if err != nil {
		t.Fatal(err)
	}
	if meResp.StatusCode != fiber.StatusOK {
		t.Fatalf("me status %d", meResp.StatusCode)
	}
	meRaw, err := io.ReadAll(meResp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(meRaw), uid) {
		t.Fatalf("uid leaked on /me: %s", meRaw)
	}
	var me map[string]any
	if err := json.Unmarshal(meRaw, &me); err != nil {
		t.Fatal(err)
	}
	if _, ok := me["studentCardUid"]; ok {
		t.Fatalf("studentCardUid key on /me: %s", meRaw)
	}
}
