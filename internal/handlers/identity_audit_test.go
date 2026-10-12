package handlers

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/requestid"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authn"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/identity"
	"github.com/skylab-kulubu/core-backend/internal/teamaudit"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// auditedApp is the identity routes over dir with the Team audit record in
// front of its writes, as main wires them.
func auditedApp(t *testing.T, ident authn.Identity, dir *identity.Memory, audit teamaudit.Writer) *fiber.App {
	t.Helper()
	svc := identity.NewServiceWithOptions(identity.AuditedDirectory(dir, audit, t.Logf), user.NewMemoryStore(),
		authz.NewAuthorizer(authz.DefaultPolicy()), identity.Options{})
	h := NewIdentityHandler(svc)
	app := fiber.New()
	app.Use(requestid.New())
	app.Use(func(c fiber.Ctx) error {
		c.Locals(authn.LocalsIdentity, ident)
		return c.Next()
	})
	app.Post("/v1/groups", h.CreateGroup)
	app.Patch("/v1/groups/:groupId", h.UpdateGroup)
	app.Post("/v1/groups/:groupId/members", h.AddMember)
	app.Delete("/v1/groups/:groupId/members/:userId", h.RemoveMember)
	app.Put("/v1/groups/:groupId/client-roles", h.SetGroupClientRoles)
	return app
}

type auditFixture struct {
	dir                   *identity.Memory
	uyeler, weblab, agc   identity.Group
	member, sub, newcomer uuid.UUID
}

// auditTree is /UYELER/ARGE/WEBLAB with a sub-team AGC; member sits in
// WEBLAB, sub in AGC.
func auditTree() auditFixture {
	f := auditFixture{dir: identity.NewMemory()}
	f.uyeler = identity.Group{ID: uuid.NewString(), Name: "UYELER", Path: "/UYELER"}
	arge := identity.Group{ID: uuid.NewString(), Name: "ARGE", Path: "/UYELER/ARGE"}
	f.weblab = identity.Group{ID: uuid.NewString(), Name: "WEBLAB", Path: "/UYELER/ARGE/WEBLAB",
		Attributes: map[string]string{"display_name_tr": "Web", "public_listing": "true", "note": "x"}}
	f.agc = identity.Group{ID: uuid.NewString(), Name: "AGC", Path: "/UYELER/ARGE/WEBLAB/AGC"}
	for _, g := range []identity.Group{f.uyeler, arge, f.weblab, f.agc} {
		f.dir.PutGroup(g)
	}
	f.member, f.sub, f.newcomer = uuid.New(), uuid.New(), uuid.New()
	for _, id := range []uuid.UUID{f.member, f.sub, f.newcomer} {
		f.dir.PutUser(identity.Person{ID: id, Email: id.String() + "@example.test", FirstName: "Ada"})
	}
	ctx := context.Background()
	_ = f.dir.AddMember(ctx, f.weblab.ID, f.member)
	_ = f.dir.AddMember(ctx, f.agc.ID, f.sub)
	_ = f.dir.SetGroupClientRoles(ctx, f.weblab.ID, []identity.ClientRole{{ClientID: "core", Role: "events:manage"}})
	f.dir.Ops = nil
	return f
}

func auditRequest(t *testing.T, app *fiber.App, method, target, body string) (*http.Response, string) {
	t.Helper()
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set(fiber.HeaderXRequestID, "req-"+strings.ReplaceAll(strings.Trim(target, "/"), "/", "-"))
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

var groupWrites = []string{"CreateGroup", "UpdateGroup", "AddMember", "RemoveMember", "SetGroupClientRoles"}

func writeOps(dir *identity.Memory) []string {
	var ops []string
	for _, op := range dir.Ops {
		if slices.Contains(groupWrites, op) {
			ops = append(ops, op)
		}
	}
	return ops
}

type auditWrite struct {
	name, method, target, body string
	want                       int
	check                      func(t *testing.T, f auditFixture, row teamaudit.Row)
}

func auditWrites(f auditFixture) []auditWrite {
	return []auditWrite{
		{"create", fiber.MethodPost, "/v1/groups", `{"name":"NEWTEAM","parentId":"` + f.weblab.ID + `"}`, fiber.StatusCreated,
			func(t *testing.T, f auditFixture, row teamaudit.Row) {
				if row.Action != teamaudit.ActionTeamCreate || row.GroupPath != "/UYELER/ARGE/WEBLAB/NEWTEAM" || row.GroupID == "" ||
					row.After == nil || row.After.ID != row.GroupID || row.After.ParentID != f.weblab.ID || row.After.Code != "NEWTEAM" {
					t.Fatalf("create row %+v after %+v", row, row.After)
				}
			}},
		{"attributes", fiber.MethodPatch, "/v1/groups/" + f.weblab.ID,
			`{"attributes":{"display_name_tr":"Web Lab","public_listing":"true","note":"y"}}`, fiber.StatusOK,
			func(t *testing.T, f auditFixture, row teamaudit.Row) {
				if row.Action != teamaudit.ActionGroupAttributes || row.GroupID != f.weblab.ID || row.Before.Code != "WEBLAB" ||
					row.Before.Attributes["display_name_tr"] != "Web" || row.After.Attributes["display_name_tr"] != "Web Lab" ||
					len(row.After.Attributes) != 1 || !slices.Equal(row.After.OtherAttributes, []string{"note"}) {
					t.Fatalf("attributes row before %+v after %+v", row.Before, row.After)
				}
			}},
		{"rename", fiber.MethodPatch, "/v1/groups/" + f.agc.ID, `{"name":"AGC2"}`, fiber.StatusOK,
			func(t *testing.T, f auditFixture, row teamaudit.Row) {
				if row.Action != teamaudit.ActionTeamCode || row.GroupPath != f.agc.Path || row.Before.Code != "AGC" ||
					row.After.Code != "AGC2" || row.After.Path != "/UYELER/ARGE/WEBLAB/AGC2" {
					t.Fatalf("rename row before %+v after %+v", row.Before, row.After)
				}
			}},
		{"add member", fiber.MethodPost, "/v1/groups/" + f.weblab.ID + "/members", `{"userId":"` + f.newcomer.String() + `"}`, fiber.StatusNoContent,
			func(t *testing.T, f auditFixture, row teamaudit.Row) {
				if row.Action != teamaudit.ActionMemberAdd || row.GroupID != f.weblab.ID || row.TargetUserID != f.newcomer || row.Before != nil || row.After != nil {
					t.Fatalf("add row %+v", row)
				}
			}},
		{"remove through the parent roster", fiber.MethodDelete, "/v1/groups/" + f.weblab.ID + "/members/" + f.sub.String(), "", fiber.StatusNoContent,
			func(t *testing.T, f auditFixture, row teamaudit.Row) {
				// The Source group the membership came from, not the
				// roster's Group.
				if row.Action != teamaudit.ActionMemberRemove || row.GroupID != f.agc.ID || row.GroupPath != f.agc.Path || row.TargetUserID != f.sub {
					t.Fatalf("remove row %+v", row)
				}
			}},
		{"roles", fiber.MethodPut, "/v1/groups/" + f.weblab.ID + "/client-roles", `[{"clientId":"core","role":"media:attach"},{"clientId":"cms","role":"access"}]`, fiber.StatusNoContent,
			func(t *testing.T, f auditFixture, row teamaudit.Row) {
				if row.Action != teamaudit.ActionGroupRoles || row.GroupID != f.weblab.ID ||
					!slices.Equal(row.Before.Roles, []string{"core:events:manage"}) ||
					!slices.Equal(row.After.Roles, []string{"cms:access", "core:media:attach"}) {
					t.Fatalf("roles row before %+v after %+v", row.Before, row.After)
				}
			}},
	}
}

// Every Group write of Privileged leaves exactly one row, finished ok, with
// the caller as the actor on a privileged basis, the token's client and the
// request's id.
func TestPrivilegedGroupWritesLeaveOneAuditRowEach(t *testing.T) {
	t.Parallel()
	f := auditTree()
	audit := teamaudit.NewMemory()
	ident := ykIdent()
	ident.Client = "admin-panel"
	app := auditedApp(t, ident, f.dir, audit)
	for i, w := range auditWrites(f) {
		resp, body := auditRequest(t, app, w.method, w.target, w.body)
		if resp.StatusCode != w.want {
			t.Fatalf("%s: status %d %s", w.name, resp.StatusCode, body)
		}
		rows := audit.Rows()
		if len(rows) != i+1 {
			t.Fatalf("%s: %d rows after %d writes", w.name, len(rows), i+1)
		}
		row := rows[i]
		if row.Outcome != teamaudit.OutcomeOK || row.ErrorCode != "" || row.Actor.ID != ident.ID ||
			row.Actor.Basis != teamaudit.BasisPrivileged || row.Actor.ViaGroupID != "" || row.Actor.Client != "admin-panel" ||
			row.Actor.CorrelationID != resp.Header.Get(fiber.HeaderXRequestID) || row.Actor.CorrelationID == "" {
			t.Fatalf("%s: row %+v", w.name, row)
		}
		w.check(t, f, row)
	}
}

// With the audit record down every Group write answers 503
// audit_unavailable and no write reaches the directory.
func TestGroupWritesStopWhenTheAuditRecordIsDown(t *testing.T) {
	t.Parallel()
	f := auditTree()
	audit := teamaudit.NewMemory()
	audit.SetUnavailable(true)
	app := auditedApp(t, ykIdent(), f.dir, audit)
	for _, w := range auditWrites(f) {
		resp, body := auditRequest(t, app, w.method, w.target, w.body)
		var problem struct {
			Code string `json:"code"`
		}
		_ = json.Unmarshal([]byte(body), &problem)
		if resp.StatusCode != fiber.StatusServiceUnavailable || problem.Code != "audit_unavailable" ||
			resp.Header.Get(fiber.HeaderCacheControl) != "no-store" {
			t.Fatalf("%s: status %d %s", w.name, resp.StatusCode, body)
		}
	}
	if ops := writeOps(f.dir); len(ops) != 0 {
		t.Fatalf("writes reached the directory: %v", ops)
	}
	if len(audit.Rows()) != 0 {
		t.Fatal("rows written while down")
	}
}

// A caller whose subject is not an id carries no actor: no row, no change.
func TestGroupWriteWithoutAnActorIsRefused(t *testing.T) {
	t.Parallel()
	f := auditTree()
	audit := teamaudit.NewMemory()
	app := auditedApp(t, authn.Identity{Groups: []string{"/UYELER/YK"}}, f.dir, audit)
	resp, body := auditRequest(t, app, fiber.MethodPost, "/v1/groups/"+f.weblab.ID+"/members", `{"userId":"`+f.newcomer.String()+`"}`)
	if resp.StatusCode != fiber.StatusServiceUnavailable || len(writeOps(f.dir)) != 0 || len(audit.Rows()) != 0 {
		t.Fatalf("status %d %s ops %v", resp.StatusCode, body, writeOps(f.dir))
	}
}

// A write Keycloak refuses finishes its row as failed with the code; one
// authorization refuses writes no row (ticket 04 writes denied rows).
func TestFailedAndForbiddenGroupWrites(t *testing.T) {
	t.Parallel()
	f := auditTree()
	audit := teamaudit.NewMemory()
	app := auditedApp(t, ykIdent(), f.dir, audit)
	resp, body := auditRequest(t, app, fiber.MethodPost, "/v1/groups/"+f.weblab.ID+"/members", `{"userId":"`+uuid.NewString()+`"}`)
	if resp.StatusCode != fiber.StatusNotFound {
		t.Fatalf("status %d %s", resp.StatusCode, body)
	}
	rows := audit.Rows()
	if len(rows) != 1 || rows[0].Outcome != teamaudit.OutcomeFailed || rows[0].ErrorCode != "not_found" {
		t.Fatalf("rows %+v", rows)
	}

	member := authn.Identity{ID: f.member, Groups: []string{f.weblab.Path}}
	forbidden := auditedApp(t, member, f.dir, audit)
	resp, _ = auditRequest(t, forbidden, fiber.MethodPost, "/v1/groups/"+f.weblab.ID+"/members", `{"userId":"`+f.newcomer.String()+`"}`)
	if resp.StatusCode != fiber.StatusForbidden || len(audit.Rows()) != 1 {
		t.Fatalf("forbidden: status %d rows %d", resp.StatusCode, len(audit.Rows()))
	}
}
