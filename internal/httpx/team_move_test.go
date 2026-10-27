package httpx_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/skylab-kulubu/core-backend/internal/identity"
	"github.com/skylab-kulubu/core-backend/internal/teamaudit"
	"github.com/skylab-kulubu/core-backend/internal/teamcode"
)

func (e *delegationEnv) move(as, path, parent string) rawResponse {
	return e.send(e.token(as), fiber.MethodPost, "/v1/groups/"+e.idOf(path)+"/move", `{"parentId":"`+e.idOf(parent)+`"}`)
}

func (e *delegationEnv) preview(as, path, parent string) (rawResponse, identity.MovePreview) {
	got := e.send(e.token(as), fiber.MethodGet, "/v1/groups/"+e.idOf(path)+"/move-preview?parentId="+e.idOf(parent), "")
	var out identity.MovePreview
	_ = json.Unmarshal(got.body, &out)
	return got, out
}

// K4: a Sub-team moves under another Team when the caller manages both
// sides: the Team it leaves strictly and the one it joins. Privileged moves
// any Team, a top one to another area. A team.move row with the path and
// the parent before and after.
func TestTeamMove(t *testing.T) {
	t.Parallel()
	e := newDelegationEnv(t, delegationSetup{on: true})
	wantStatus(t, "open BACK", e.createSub("webLeader", "/UYELER/ARGE/WEBLAB", "BACK"), fiber.StatusCreated, "")
	back := e.idOf("/UYELER/ARGE/WEBLAB/BACK")

	before := len(e.audit.Rows())
	wantStatus(t, "their common upper Leader", e.move("webLeader", "/UYELER/ARGE/WEBLAB/BACK", "/UYELER/ARGE/WEBLAB/FRONT"), fiber.StatusOK, "")
	if e.idOf("/UYELER/ARGE/WEBLAB/FRONT/BACK") != back {
		t.Fatal("BACK did not move under FRONT")
	}
	rows := e.rowsSince(before)
	if len(rows) != 1 || rows[0].Action != teamaudit.ActionTeamMove || rows[0].Outcome != teamaudit.OutcomeOK ||
		rows[0].Actor.Basis != teamaudit.BasisLeader || rows[0].Actor.ViaGroupID != e.ids["/UYELER/ARGE/WEBLAB"] ||
		rows[0].Before.Path != "/UYELER/ARGE/WEBLAB/BACK" || rows[0].Before.ParentID != e.ids["/UYELER/ARGE/WEBLAB"] ||
		rows[0].After.Path != "/UYELER/ARGE/WEBLAB/FRONT/BACK" || rows[0].After.ParentID != e.ids["/UYELER/ARGE/WEBLAB/FRONT"] {
		t.Errorf("rows %+v, want one team.move row", rows)
	}

	before = len(e.audit.Rows())
	for _, c := range []struct{ name, as, path, parent, code string }{
		{"only the source (FRONT's Leader to WEBLAB)", "frontLeader", "/UYELER/ARGE/WEBLAB/FRONT/BACK", "/UYELER/ARGE/WEBLAB", ""},
		{"only the target (MOBILAB's Leader takes FRONT)", "mobLeader", "/UYELER/ARGE/WEBLAB/FRONT", "/UYELER/ARGE/MOBILAB", ""},
		{"only the source (WEBLAB's Leader gives FRONT away)", "webLeader", "/UYELER/ARGE/WEBLAB/FRONT", "/UYELER/ARGE/MOBILAB", ""},
		{"a Sub-team's Leader moves their own", "frontLeader", "/UYELER/ARGE/WEBLAB/FRONT", "/UYELER/ARGE/WEBLAB/FRONT/BACK", ""},
		{"a top Team's Leader moves their own", "webLeader", "/UYELER/ARGE/WEBLAB", "/UYELER/ORGANIZASYON", ""},
		{"a Leader to an area", "webLeader", "/UYELER/ARGE/WEBLAB/FRONT/BACK", "/UYELER/ARGE", ""},
		{"a member", "member", "/UYELER/ARGE/WEBLAB/FRONT/BACK", "/UYELER/ARGE/WEBLAB", ""},
		{"under a protected Team", "webLeader", "/UYELER/ARGE/WEBLAB/FRONT/BACK", "/UYELER/ARGE/WEBLAB/SECURE", "protected_group"},
	} {
		wantStatus(t, c.name, e.move(c.as, c.path, c.parent), fiber.StatusForbidden, c.code)
	}
	for _, row := range e.rowsSince(before) {
		if row.Outcome != teamaudit.OutcomeDenied || row.Action != teamaudit.ActionTeamMove {
			t.Errorf("a refusal wrote %+v", row)
		}
	}
	if e.idOf("/UYELER/ARGE/WEBLAB/FRONT/BACK") != back || e.idOf("/UYELER/ARGE/WEBLAB/FRONT") == "" {
		t.Fatal("a refused move moved")
	}
	wantStatus(t, "a Leader, under a Leader group", e.move("webLeader", "/UYELER/ARGE/WEBLAB/FRONT/BACK", "/UYELER/ARGE/WEBLAB/LIDERLER"), fiber.StatusConflict, identity.CodeLeaderGroupTarget)
	wantStatus(t, "a Leader, into its own subtree", e.move("webLeader", "/UYELER/ARGE/WEBLAB/FRONT", "/UYELER/ARGE/WEBLAB/FRONT/BACK"), fiber.StatusConflict, identity.CodeCycle)
	wantStatus(t, "a Leader, to where it is", e.move("webLeader", "/UYELER/ARGE/WEBLAB/FRONT/BACK", "/UYELER/ARGE/WEBLAB/FRONT"), fiber.StatusConflict, identity.CodeSameParent)
	wantStatus(t, "a parent by path", e.send(e.token("webLeader"), fiber.MethodPost, "/v1/groups/"+back+"/move", `{"parentId":"/UYELER/ARGE/WEBLAB"}`), fiber.StatusBadRequest, "")
	wantStatus(t, "YK, a parent by path", e.send(e.token("yk"), fiber.MethodPost, "/v1/groups/"+back+"/move", `{"parentId":"WEBLAB"}`), fiber.StatusBadRequest, "")

	// Privileged: a Sub-team across Teams, a top Team across areas.
	wantStatus(t, "YK moves FRONT to MOBILAB", e.move("yk", "/UYELER/ARGE/WEBLAB/FRONT", "/UYELER/ARGE/MOBILAB"), fiber.StatusOK, "")
	if e.idOf("/UYELER/ARGE/MOBILAB/FRONT/LIDERLER") == "" || e.idOf("/UYELER/ARGE/MOBILAB/FRONT/BACK") != back {
		t.Fatal("FRONT's subtree did not move with it")
	}
	// Delegation follows at once: WEBLAB's Leader no longer manages FRONT,
	// MOBILAB's does.
	wantStatus(t, "WEBLAB's Leader after the move", e.send(e.token("webLeader"), fiber.MethodPost, "/v1/groups/"+e.ids["/UYELER/ARGE/WEBLAB/FRONT"]+"/members",
		`{"userId":"`+e.people["newcomer"].String()+`"}`), fiber.StatusForbidden, "")
	wantStatus(t, "MOBILAB's Leader after the move", e.send(e.token("mobLeader"), fiber.MethodPost, "/v1/groups/"+e.ids["/UYELER/ARGE/WEBLAB/FRONT"]+"/members",
		`{"userId":"`+e.people["newcomer"].String()+`"}`), fiber.StatusNoContent, "")
	wantStatus(t, "YK moves a top Team to another area", e.move("yk", "/UYELER/ARGE/WEBLAB", "/UYELER/ORGANIZASYON"), fiber.StatusOK, "")
	if e.idOf("/UYELER/ORGANIZASYON/WEBLAB/LIDERLER") == "" {
		t.Fatal("WEBLAB did not move")
	}
}

// The rules hold for Privileged too (SPEC §7; review of #227, Y4): no
// cycle, no Leader group or Group outside the Teams as a parent, nothing
// from, to or into a Privileged root, the structure limits.
func TestTeamMoveRules(t *testing.T) {
	t.Parallel()
	e := newDelegationEnv(t, delegationSetup{on: true})
	wantStatus(t, "open FRONT-UI", e.createSub("yk", "/UYELER/ARGE/WEBLAB/FRONT", "FRONT-UI"), fiber.StatusCreated, "")
	e.put("/UYELER/ARGE/WEBLAB/ADMIN")
	e.put("/UYELER/YK/SEKRETER")
	e.put("/UYELER/ARGE/MOBILAB/M1")
	e.put("/UYELER/ARGE/MOBILAB/M1/M2")
	e.put("/UYELER/ARGE/MOBILAB/M3")
	e.put("/OUTSIDE")
	for _, c := range []struct{ name, path, parent, code string }{
		{"under itself", "/UYELER/ARGE/WEBLAB", "/UYELER/ARGE/WEBLAB", identity.CodeCycle},
		{"under its child", "/UYELER/ARGE/WEBLAB", "/UYELER/ARGE/WEBLAB/FRONT", identity.CodeCycle},
		{"under its grandchild", "/UYELER/ARGE/WEBLAB", "/UYELER/ARGE/WEBLAB/FRONT/FRONT-UI", identity.CodeCycle},
		{"under a Leader group", "/UYELER/ARGE/MOBILAB/M3", "/UYELER/ARGE/WEBLAB/LIDERLER", identity.CodeLeaderGroupTarget},
		{"to a Privileged root", "/UYELER/ARGE/WEBLAB/FRONT", "/UYELER/YK", teamcode.PrivilegedRoot},
		{"from a Privileged root", "/UYELER/YK/SEKRETER", "/UYELER/ARGE/WEBLAB", teamcode.PrivilegedRoot},
		{"making a root (ADMIN under /UYELER)", "/UYELER/ARGE/WEBLAB/ADMIN", "/UYELER", teamcode.PrivilegedRoot},
		{"under the members' tree", "/UYELER/ARGE/MOBILAB/M3", "/UYELER", identity.CodeInvalidMoveTarget},
		{"outside the Teams", "/UYELER/ARGE/MOBILAB/M3", "/OUTSIDE", identity.CodeInvalidMoveTarget},
		{"an area", "/UYELER/ARGE", "/UYELER/ORGANIZASYON", identity.CodeInvalidMoveSource},
		{"a Leader group", "/UYELER/ARGE/WEBLAB/LIDERLER", "/UYELER/ARGE/MOBILAB", identity.CodeInvalidMoveSource},
		{"too deep with its Sub-team", "/UYELER/ARGE/MOBILAB/M1", "/UYELER/ARGE/WEBLAB/FRONT", identity.CodeDepthLimit},
		{"to where it is", "/UYELER/ARGE/MOBILAB/M3", "/UYELER/ARGE/MOBILAB", identity.CodeSameParent},
	} {
		got := e.move("yk", c.path, c.parent)
		wantStatus(t, c.name, got, fiber.StatusConflict, c.code)
		if got.status == fiber.StatusOK {
			t.Fatalf("%s: moved", c.name)
		}
	}
	if e.idOf("/UYELER/ADMIN") != "" || e.idOf("/UYELER/ARGE/WEBLAB/ADMIN") == "" {
		t.Fatal("a refused move made a root")
	}

	small := newDelegationEnv(t, delegationSetup{on: true, subTeams: 2})
	small.put("/UYELER/ARGE/MOBILAB/M3")
	wantStatus(t, "a third Sub-team", small.move("yk", "/UYELER/ARGE/MOBILAB/M3", "/UYELER/ARGE/WEBLAB"), fiber.StatusConflict, identity.CodeChildrenLimit)
}

// The preview says what a move would do: the people moved, the paths, the
// roles the Groups above give before and after, the new parent's Leaders;
// a rule the move breaks is a refusal, not an error. It takes the move's
// decision.
func TestTeamMovePreview(t *testing.T) {
	t.Parallel()
	e := newDelegationEnv(t, delegationSetup{on: true})
	ctx := context.Background()
	if err := e.dir.SetGroupClientRoles(ctx, e.ids["/UYELER/ARGE/WEBLAB"], []identity.ClientRole{{ClientID: "forms", Role: "skyforms:access"}}); err != nil {
		t.Fatal(err)
	}
	if err := e.dir.SetGroupClientRoles(ctx, e.ids["/UYELER/ARGE/MOBILAB"], []identity.ClientRole{{ClientID: "skymail", Role: "skymail:access"}}); err != nil {
		t.Fatal(err)
	}
	got, p := e.preview("yk", "/UYELER/ARGE/WEBLAB/FRONT", "/UYELER/ARGE/MOBILAB")
	wantStatus(t, "YK", got, fiber.StatusOK, "")
	if !p.Allowed || p.Refusal != "" || p.OldPath != "/UYELER/ARGE/WEBLAB/FRONT" || p.NewPath != "/UYELER/ARGE/MOBILAB/FRONT" ||
		p.Members != 2 || p.NewParentLeaders != 1 || len(p.RolesGained) != 1 || p.RolesGained[0] != "skymail:skymail:access" ||
		len(p.RolesLost) != 1 || p.RolesLost[0] != "forms:skyforms:access" {
		t.Errorf("preview %+v", p)
	}
	if e.idOf("/UYELER/ARGE/WEBLAB/FRONT") == "" || len(e.audit.Rows()) != 0 {
		t.Fatal("a preview moved or wrote")
	}
	got, p = e.preview("yk", "/UYELER/ARGE/WEBLAB", "/UYELER/ARGE/WEBLAB/FRONT")
	wantStatus(t, "a cycle", got, fiber.StatusOK, "")
	if p.Allowed || p.Refusal != identity.CodeCycle {
		t.Errorf("preview %+v", p)
	}
	wantStatus(t, "open BACK", e.createSub("webLeader", "/UYELER/ARGE/WEBLAB", "BACK"), fiber.StatusCreated, "")
	got, p = e.preview("webLeader", "/UYELER/ARGE/WEBLAB/BACK", "/UYELER/ARGE/WEBLAB/FRONT")
	wantStatus(t, "a Leader of both sides", got, fiber.StatusOK, "")
	if !p.Allowed || p.Members != 0 || p.NewParentLeaders != 1 {
		t.Errorf("preview %+v", p)
	}
	got, _ = e.preview("webLeader", "/UYELER/ARGE/WEBLAB/FRONT", "/UYELER/ARGE/MOBILAB")
	wantStatus(t, "a Leader of one side", got, fiber.StatusForbidden, "")
	got, _ = e.preview("frontLeader", "/UYELER/ARGE/WEBLAB/FRONT", "/UYELER/ARGE/WEBLAB/BACK")
	wantStatus(t, "a Sub-team's own Leader", got, fiber.StatusForbidden, "")
	got, _ = e.preview("member", "/UYELER/ARGE/WEBLAB/BACK", "/UYELER/ARGE/WEBLAB/FRONT")
	wantStatus(t, "a member", got, fiber.StatusForbidden, "")
}

// With delegation off the routes are Privileged's.
func TestTeamMoveDelegationOff(t *testing.T) {
	t.Parallel()
	e := newDelegationEnv(t, delegationSetup{})
	wantStatus(t, "a Leader", e.move("webLeader", "/UYELER/ARGE/WEBLAB/FRONT", "/UYELER/ARGE/WEBLAB/SECURE"), fiber.StatusForbidden, "")
	got, _ := e.preview("webLeader", "/UYELER/ARGE/WEBLAB/FRONT", "/UYELER/ARGE/WEBLAB/SECURE")
	wantStatus(t, "a Leader's preview", got, fiber.StatusForbidden, "")
	if rows := e.audit.Rows(); len(rows) != 0 {
		t.Errorf("rows %+v", rows)
	}
	wantStatus(t, "YK", e.move("yk", "/UYELER/ARGE/WEBLAB/FRONT", "/UYELER/ARGE/MOBILAB"), fiber.StatusOK, "")
}
