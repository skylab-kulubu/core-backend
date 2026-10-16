package httpx_test

import (
	"context"
	"testing"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/httpx"
	"github.com/skylab-kulubu/core-backend/internal/identity"
	"github.com/skylab-kulubu/core-backend/internal/teamaudit"
	"github.com/skylab-kulubu/core-backend/internal/teamcode"
	"github.com/skylab-kulubu/core-backend/internal/testauth"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

func TestGroupDeleteAssembledRoute(t *testing.T) {
	keys := testauth.New(t)
	dir := identity.NewMemory()
	root := identity.Group{ID: uuid.NewString(), Name: "ADMIN", Path: "/ADMIN"}
	dir.PutGroup(root)
	_ = dir.SetGroupClientRoles(context.Background(), root.ID, []identity.ClientRole{{ClientID: "core", Role: "groups:manage"}})
	group := identity.Group{ID: uuid.NewString(), Name: "TANISMAYEMEGIGRUBU", Path: "/TANISMAYEMEGIGRUBU"}
	dir.PutGroup(group)
	ledger, audit := teamcode.NewMemoryLedger(), teamaudit.NewMemory()
	deps := memoryDepsOver(dir, identity.Options{TeamCodes: ledger})
	deps.ParseToken = keys.Parse()
	deps.Identity = identity.NewServiceWithOptions(identity.AuditedDirectory(dir, audit, t.Logf), user.NewMemoryStore(), authz.NewAuthorizer(authz.DefaultPolicy()), identity.Options{TeamCodes: ledger})
	app := httpx.New(deps)
	token := func(groups ...string) string {
		return keys.Token(t, jwt.MapClaims{"sub": uuid.NewString(), "email": "example@example.test", "given_name": "Test", "family_name": "User", "groups": groups})
	}
	for _, tc := range []struct {
		token string
		want  int
	}{
		{"", 401}, {token("/DK"), 403}, {token("/UYELER/ARGE/WEBLAB/LIDERLER"), 403}, {token("/ADMIN"), 204}, {token("/ADMIN"), 404},
	} {
		if got := statusOf(t, app, tc.token, "DELETE", "/v1/groups/"+group.ID, ""); got != tc.want {
			t.Fatalf("status%d want%d", got, tc.want)
		}
	}
	if len(audit.Rows()) != 1 || audit.Rows()[0].Outcome != teamaudit.OutcomeOK {
		t.Fatal("successful route did not record deletion")
	}
}
