package httpx_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/httpx"
	"github.com/skylab-kulubu/core-backend/internal/identity"
	"github.com/skylab-kulubu/core-backend/internal/teamaudit"
	"github.com/skylab-kulubu/core-backend/internal/testauth"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// The person card's account routes (docs/user-accounts.md): ADMIN reads
// and sets a temporary password, which the answer carries once and no
// cache keeps; DK reads and is refused the write; a member reads nothing;
// nobody changes their own account here.
func TestUserAccountRoutes(t *testing.T) {
	keys := testauth.New(t)
	dir := identity.NewMemory()
	deps := memoryDepsOver(dir, identity.Options{})
	deps.ParseToken = keys.Parse()
	accounts := identity.NewMemoryAccounts()
	audit := teamaudit.NewMemory()
	deps.UserAccounts = identity.NewUserAccounts(identity.UserAccountOptions{
		Accounts: accounts, People: dir, Users: user.NewMemoryStore(), Authz: authz.NewAuthorizer(authz.DefaultPolicy()), Audit: audit,
	})
	app := httpx.New(deps)
	target := uuid.New()
	dir.PutUser(identity.Person{ID: target, Email: "ada@example.com", FirstName: "Ada", LastName: "Lovelace"})
	accounts.PutAccount(identity.AccountUser{ID: target, Username: "ada", Email: "ada@example.com", Enabled: true})
	adminID := uuid.NewString()
	admin := groupToken(t, keys, adminID, "/ADMIN")
	dk := groupToken(t, keys, uuid.NewString(), "/UYELER/DK")
	member := groupToken(t, keys, uuid.NewString(), "/UYELER/ARGE/WEBLAB")

	read := audienceCall(t, app, admin, fiber.MethodGet, "/v1/users/"+target.String()+"/account", "")
	if read.status != fiber.StatusOK || read.header.Get(fiber.HeaderCacheControl) != "no-store" {
		t.Fatalf("read %d %s", read.status, read.raw)
	}
	var account identity.Account
	if err := json.Unmarshal(read.raw, &account); err != nil || !account.Can.Manage || account.Username != "ada" {
		t.Fatalf("account %s %v", read.raw, err)
	}
	if got := audienceCall(t, app, member, fiber.MethodGet, "/v1/users/"+target.String()+"/account", ""); got.status != fiber.StatusForbidden {
		t.Fatalf("member read %d", got.status)
	}
	if got := audienceCall(t, app, dk, fiber.MethodGet, "/v1/users/"+target.String()+"/account", ""); got.status != fiber.StatusOK {
		t.Fatalf("DK read %d", got.status)
	}

	set := audienceCall(t, app, admin, fiber.MethodPost, "/v1/users/"+target.String()+"/password", "")
	if set.status != fiber.StatusOK || set.header.Get(fiber.HeaderCacheControl) != "no-store" {
		t.Fatalf("password %d %s", set.status, set.raw)
	}
	var result identity.PasswordResult
	if err := json.Unmarshal(set.raw, &result); err != nil || !result.Generated || result.Password == "" || !result.Temporary {
		t.Fatalf("password %s", set.raw)
	}
	if got := audienceCall(t, app, dk, fiber.MethodPost, "/v1/users/"+target.String()+"/password", `{"password":"Parola-12345"}`); got.status != fiber.StatusForbidden {
		t.Fatalf("DK password %d", got.status)
	}
	own := audienceCall(t, app, admin, fiber.MethodPut, "/v1/users/"+adminID+"/enabled", `{"enabled":false}`)
	if own.status != fiber.StatusConflict || !strings.Contains(string(own.raw), identity.AccountOwn) {
		t.Fatalf("own %d %s", own.status, own.raw)
	}
	if got := audienceCall(t, app, admin, fiber.MethodPut, "/v1/users/"+target.String()+"/enabled", `{"enabled":false,"extra":1}`); got.status != fiber.StatusBadRequest {
		t.Fatalf("unknown field %d", got.status)
	}
	bad := audienceCall(t, app, admin, fiber.MethodPut, "/v1/users/"+target.String()+"/email", `{"address":"ada@std.yildiz.edu.tr"}`)
	if bad.status != fiber.StatusUnprocessableEntity || !strings.Contains(string(bad.raw), identity.AccountSchoolDomain) {
		t.Fatalf("school domain %d %s", bad.status, bad.raw)
	}
	if got := audienceCall(t, app, admin, fiber.MethodPut, "/v1/users/"+target.String()+"/enabled", `{"enabled":false}`); got.status != fiber.StatusNoContent {
		t.Fatalf("disable %d %s", got.status, got.raw)
	}
	if u, _ := accounts.Get(target); u.Enabled {
		t.Fatal("not disabled")
	}
	for _, row := range audit.Rows() {
		raw, _ := json.Marshal(row)
		if strings.Contains(string(raw), result.Password) {
			t.Fatalf("a row holds the password: %s", raw)
		}
	}
	if _, err := accounts.AccountUser(context.Background(), target); err != nil {
		t.Fatal(err)
	}
}
