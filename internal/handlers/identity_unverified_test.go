package handlers

import (
	"context"
	"encoding/json"
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
	"github.com/skylab-kulubu/core-backend/internal/ytu"
)

// unverifiedFixture: a YK Group and a team, a person who is YTÜ-linked and
// two who are not (one core has no account of).
type unverifiedFixture struct {
	app                         *fiber.App
	dir                         *identity.Memory
	audit                       *teamaudit.Memory
	yk, baskan, weblab          identity.Group
	linked, unlinked, noAccount uuid.UUID
}

func newUnverifiedFixture(t *testing.T) unverifiedFixture {
	t.Helper()
	return newUnverifiedFixtureAs(t, ykIdent())
}

// newUnverifiedFixtureAs is the fixture with the routes called as caller.
func newUnverifiedFixtureAs(t *testing.T, caller authn.Identity) unverifiedFixture {
	t.Helper()
	f := unverifiedFixture{dir: identity.NewMemory(), audit: teamaudit.NewMemory()}
	f.yk = identity.Group{ID: uuid.NewString(), Name: "YK", Path: "/UYELER/YK"}
	f.weblab = identity.Group{ID: uuid.NewString(), Name: "WEBLAB", Path: "/UYELER/ARGE/WEBLAB"}
	f.baskan = identity.Group{ID: uuid.NewString(), Name: "BASKAN", Path: "/UYELER/YK/BASKAN"}
	for _, g := range []identity.Group{{ID: uuid.NewString(), Name: "UYELER", Path: "/UYELER"}, {ID: uuid.NewString(), Name: "ARGE", Path: "/UYELER/ARGE"}, f.yk, f.baskan, f.weblab} {
		f.dir.PutGroup(g)
	}
	users := user.NewMemoryStore()
	ctx := context.Background()
	f.linked, f.unlinked, f.noAccount = uuid.New(), uuid.New(), uuid.New()
	for _, id := range []uuid.UUID{f.linked, f.unlinked, f.noAccount} {
		f.dir.PutUser(identity.Person{ID: id, Email: id.String() + "@example.test", FirstName: "Ada", Enabled: true})
	}
	for _, id := range []uuid.UUID{f.linked, f.unlinked} {
		if _, _, err := users.Upsert(ctx, user.User{ID: id, Email: id.String() + "@example.test", FirstName: "Ada", LastName: "Lovelace"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := users.SetYTUProfile(ctx, f.linked, ytu.Profile{University: "Yıldız Teknik Üniversitesi"}); err != nil {
		t.Fatal(err)
	}
	svc := identity.NewServiceWithOptions(identity.AuditedDirectory(f.dir, f.audit, t.Logf), users,
		authz.NewAuthorizer(authz.DefaultPolicy()), identity.Options{})
	h := NewIdentityHandler(svc)
	f.app = fiber.New()
	f.app.Use(requestid.New())
	f.app.Use(func(c fiber.Ctx) error {
		c.Locals(authn.LocalsIdentity, caller)
		return c.Next()
	})
	f.app.Post("/v1/groups/:groupId/members", h.AddMember)
	f.app.Get("/v1/groups/:groupId/member-check/:userId", h.MemberCheck)
	return f
}

func (f unverifiedFixture) add(t *testing.T, group string, person uuid.UUID, extra string) teamaudit.Row {
	t.Helper()
	resp, body := auditRequest(t, f.app, fiber.MethodPost, "/v1/groups/"+group+"/members", `{"userId":"`+person.String()+`"`+extra+`}`)
	if resp.StatusCode != fiber.StatusNoContent {
		t.Fatalf("add: %d %s", resp.StatusCode, body)
	}
	rows := f.audit.Rows()
	return rows[len(rows)-1]
}

// The check answers only whoever may read the Group (review of #241, F3):
// an ordinary member, or a caller with no subject, learns nothing of the
// person and the Group, and nothing is written.
func TestMemberCheckRefusesWhoMayNotReadTheGroup(t *testing.T) {
	t.Parallel()
	for _, caller := range []authn.Identity{
		{ID: uuid.New(), Groups: []string{"/UYELER/ARGE/WEBLAB"}},
		{ID: uuid.New()},
	} {
		f := newUnverifiedFixtureAs(t, caller)
		for _, group := range []string{f.yk.ID, f.weblab.ID} {
			resp, body := auditRequest(t, f.app, fiber.MethodGet, "/v1/groups/"+group+"/member-check/"+f.unlinked.String(), "")
			if resp.StatusCode != fiber.StatusForbidden || strings.Contains(body, "ytuLinked") || strings.Contains(body, "warnings") {
				t.Fatalf("caller %v group %s: %d %s", caller.Groups, group, resp.StatusCode, body)
			}
		}
		if rows := f.audit.Rows(); len(rows) != 0 {
			t.Fatalf("rows %+v", rows)
		}
	}
}

// grup-yonetimi 07: adding someone who is not YTÜ-linked to a Privileged
// Group is never refused; the row records whether the caller confirmed the
// warning, and only such a row carries the mark.
func TestUnverifiedAddToAPrivilegedGroupIsMarkedNotRefused(t *testing.T) {
	t.Parallel()
	f := newUnverifiedFixture(t)

	row := f.add(t, f.yk.ID, f.unlinked, `,"unverifiedAck":true`)
	if row.Action != teamaudit.ActionMemberAdd || row.Outcome != teamaudit.OutcomeOK || row.After == nil || row.After.UnverifiedAck == nil || !*row.After.UnverifiedAck {
		t.Fatalf("confirmed add row %+v after %+v", row, row.After)
	}
	// Without the field the add still happens, marked unconfirmed; a
	// subgroup counts, and a person core has no account of is unverified.
	row = f.add(t, f.baskan.ID, f.noAccount, "")
	if row.Outcome != teamaudit.OutcomeOK || row.After == nil || row.After.UnverifiedAck == nil || *row.After.UnverifiedAck {
		t.Fatalf("unconfirmed add row after %+v", row.After)
	}
	// YTÜ-linked: no mark, whatever the request says.
	if row = f.add(t, f.yk.ID, f.linked, `,"unverifiedAck":true`); row.After != nil {
		t.Fatalf("verified add marked: %+v", row.After)
	}
	// An ordinary team: the field is ignored.
	if row = f.add(t, f.weblab.ID, f.unlinked, `,"unverifiedAck":true`); row.After != nil {
		t.Fatalf("team add marked: %+v", row.After)
	}
	encoded, _ := json.Marshal(teamaudit.State{UnverifiedAck: new(bool)})
	if string(encoded) != `{"unverifiedAck":false}` {
		t.Fatalf("after %s", encoded)
	}
}

func TestMemberCheckWarnsOfAnUnverifiedPrivilegedAdd(t *testing.T) {
	t.Parallel()
	f := newUnverifiedFixture(t)
	for _, tc := range []struct {
		group  string
		person uuid.UUID
		want   string
	}{
		{f.yk.ID, f.unlinked, `{"privileged":true,"ytuLinked":false,"warnings":["not_ytu_verified"]}`},
		{f.yk.ID, f.noAccount, `{"privileged":true,"ytuLinked":false,"warnings":["not_ytu_verified"]}`},
		{f.yk.ID, f.linked, `{"privileged":true,"ytuLinked":true,"warnings":[]}`},
		{f.weblab.ID, f.unlinked, `{"privileged":false,"ytuLinked":false,"warnings":[]}`},
	} {
		resp, body := auditRequest(t, f.app, fiber.MethodGet, "/v1/groups/"+tc.group+"/member-check/"+tc.person.String(), "")
		if resp.StatusCode != fiber.StatusOK || body != tc.want {
			t.Fatalf("check %s: %d %s, want %s", tc.group, resp.StatusCode, body, tc.want)
		}
	}
	if resp, _ := auditRequest(t, f.app, fiber.MethodGet, "/v1/groups/"+f.yk.ID+"/member-check/not-an-id", ""); resp.StatusCode != fiber.StatusBadRequest {
		t.Fatalf("bad id: %d", resp.StatusCode)
	}
	// A check writes nothing.
	if rows := f.audit.Rows(); len(rows) != 0 {
		t.Fatalf("rows %+v", rows)
	}
}
