package httpx_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/identity"
	"github.com/skylab-kulubu/core-backend/internal/teamaudit"
	"github.com/skylab-kulubu/core-backend/internal/teamcode"
)

func (e *delegationEnv) addLeader(as, path, who, kind string) rawResponse {
	body := `{"userId":"` + e.people[who].String() + `"`
	if kind != "" {
		body += `,"kind":"` + kind + `"`
	}
	return e.send(e.token(as), fiber.MethodPost, "/v1/groups/"+e.idOf(path)+"/leaders", body+"}")
}

func (e *delegationEnv) removeLeader(as, path, who string) rawResponse {
	return e.send(e.token(as), fiber.MethodDelete, "/v1/groups/"+e.idOf(path)+"/leaders/"+e.people[who].String(), "")
}

// Whoever manages a Team strictly names its Leaders and Coordinators: a
// Leader of the Team above on a Sub-team, Privileged anywhere. The Leader
// group is opened when the first one is named. A leader.add row on the
// Team with the kind; the membership mail hears of the Leader group.
func TestTeamLeaderAdd(t *testing.T) {
	t.Parallel()
	e := newDelegationEnv(t, delegationSetup{on: true})

	before, heard := len(e.audit.Rows()), len(e.heard.all())
	wantStatus(t, "a Leader of the Team above", e.addLeader("webLeader", "/UYELER/ARGE/WEBLAB/FRONT", "newcomer", ""), fiber.StatusNoContent, "")
	if !e.directlyIn("newcomer", "/UYELER/ARGE/WEBLAB/FRONT/LIDERLER") {
		t.Error("not in FRONT's LIDERLER")
	}
	rows := e.rowsSince(before)
	if len(rows) != 1 || rows[0].Action != teamaudit.ActionLeaderAdd || rows[0].Outcome != teamaudit.OutcomeOK ||
		rows[0].GroupID != e.ids["/UYELER/ARGE/WEBLAB/FRONT"] || rows[0].TargetUserID != e.people["newcomer"] ||
		rows[0].After == nil || rows[0].After.Kind != "LIDERLER" || rows[0].Actor.Basis != teamaudit.BasisLeader ||
		rows[0].Actor.ViaGroupID != e.ids["/UYELER/ARGE/WEBLAB"] {
		t.Errorf("rows %+v, want one leader.add row", rows)
	}
	if changes := e.heard.all()[heard:]; len(changes) != 1 || changes[0].Group.Path != "/UYELER/ARGE/WEBLAB/FRONT/LIDERLER" || !changes[0].Added {
		t.Errorf("heard %+v", changes)
	}

	before = len(e.audit.Rows())
	wantStatus(t, "a first Coordinator", e.addLeader("webLeader", "/UYELER/ARGE/WEBLAB/FRONT", "exLeader", "KOORDINATORLER"), fiber.StatusNoContent, "")
	if !e.directlyIn("exLeader", "/UYELER/ARGE/WEBLAB/FRONT/KOORDINATORLER") {
		t.Error("the Coordinators' Leader group was not opened")
	}
	if rows := e.rowsSince(before); len(rows) != 2 || rows[0].Action != teamaudit.ActionTeamCreate || rows[0].After.Code != "KOORDINATORLER" ||
		rows[1].Action != teamaudit.ActionLeaderAdd || rows[1].After.Kind != "KOORDINATORLER" {
		t.Errorf("rows %+v, want team.create then leader.add", rows)
	}
	before = len(e.audit.Rows())
	wantStatus(t, "again", e.addLeader("webLeader", "/UYELER/ARGE/WEBLAB/FRONT", "exLeader", "KOORDINATORLER"), fiber.StatusNoContent, "")
	if rows := e.rowsSince(before); len(rows) != 0 {
		t.Errorf("a repeat wrote %+v", rows)
	}

	// A Sub-team's Leader names the Leaders of the Sub-teams below theirs.
	wantStatus(t, "open below FRONT", e.createSub("frontLeader", "/UYELER/ARGE/WEBLAB/FRONT", "FRONT-UI"), fiber.StatusCreated, "")
	wantStatus(t, "a Sub-team's Leader below their own", e.addLeader("frontLeader", "/UYELER/ARGE/WEBLAB/FRONT/FRONT-UI", "outsider", ""), fiber.StatusNoContent, "")
	if !e.directlyIn("outsider", "/UYELER/ARGE/WEBLAB/FRONT/FRONT-UI/LIDERLER") {
		t.Error("FRONT-UI's Leader group was not opened")
	}
	wantStatus(t, "YK on a top Team", e.addLeader("yk", "/UYELER/ARGE/MOBILAB", "member", "KOORDINATORLER"), fiber.StatusNoContent, "")

	before = len(e.audit.Rows())
	for _, c := range []struct{ name, as, path, who string }{
		{"a Sub-team's Leader on their own (themselves)", "frontLeader", "/UYELER/ARGE/WEBLAB/FRONT", "frontLeader"},
		{"a Sub-team's Leader on their own", "frontLeader", "/UYELER/ARGE/WEBLAB/FRONT", "frontMember"},
		{"a top Team's Leader on their own (a co-Leader, S2)", "webLeader", "/UYELER/ARGE/WEBLAB", "frontMember"},
		{"a Coordinator on their own Team", "webCoord", "/UYELER/ARGE/WEBLAB", "webCoord"},
		{"a sibling Team's Leader", "mobLeader", "/UYELER/ARGE/WEBLAB/FRONT", "mobLeader"},
		{"a member", "frontMember", "/UYELER/ARGE/WEBLAB/FRONT", "frontMember"},
		{"on a Leader group", "webLeader", "/UYELER/ARGE/WEBLAB/FRONT/LIDERLER", "frontMember"},
	} {
		wantStatus(t, c.name, e.addLeader(c.as, c.path, c.who, ""), fiber.StatusForbidden, "")
	}
	wantStatus(t, "on a protected Sub-team", e.addLeader("webLeader", "/UYELER/ARGE/WEBLAB/SECURE", "frontMember", ""), fiber.StatusForbidden, "protected_group")
	for _, row := range e.rowsSince(before) {
		if row.Outcome != teamaudit.OutcomeDenied {
			t.Errorf("a refusal wrote %+v", row)
		}
	}
	if e.directlyIn("frontMember", "/UYELER/ARGE/WEBLAB/LIDERLER") || e.directlyIn("webCoord", "/UYELER/ARGE/WEBLAB/LIDERLER") {
		t.Error("a refused add made a Leader")
	}

	wantStatus(t, "another kind", e.addLeader("webLeader", "/UYELER/ARGE/WEBLAB/FRONT", "frontMember", "ADMIN"), fiber.StatusBadRequest, "")
	wantStatus(t, "YK on a Leader group", e.addLeader("yk", "/UYELER/ARGE/WEBLAB/LIDERLER", "frontMember", ""), fiber.StatusBadRequest, "")
	wantStatus(t, "YK on an area", e.addLeader("yk", "/UYELER/ARGE", "frontMember", ""), fiber.StatusBadRequest, "")
	unknown := e.send(e.token("webLeader"), fiber.MethodPost, "/v1/groups/"+e.ids["/UYELER/ARGE/WEBLAB/FRONT"]+"/leaders", `{"userId":"`+uuid.NewString()+`"}`)
	wantStatus(t, "no account of core", unknown, fiber.StatusNotFound, "")
}

// A delegated Leader add keeps the person within the delegated Group limit.
func TestTeamLeaderAddGroupLimit(t *testing.T) {
	t.Parallel()
	e := newDelegationEnv(t, delegationSetup{on: true, groupsCap: 1})
	wantStatus(t, "a second Group", e.addLeader("webLeader", "/UYELER/ARGE/WEBLAB/FRONT", "member", ""), fiber.StatusConflict, "group_limit")
	wantStatus(t, "a first Group", e.addLeader("webLeader", "/UYELER/ARGE/WEBLAB/FRONT", "newcomer", ""), fiber.StatusNoContent, "")
}

// The Leaders of a Team, for whoever manages it: a Leader sees the safe
// fields of a person (SPEC §6.4), Privileged everything.
func TestTeamLeaderList(t *testing.T) {
	t.Parallel()
	e := newDelegationEnv(t, delegationSetup{on: true})
	got := e.send(e.token("webLeader"), fiber.MethodGet, "/v1/groups/"+e.ids["/UYELER/ARGE/WEBLAB"]+"/leaders", "")
	wantStatus(t, "the Team's own Leader reads", got, fiber.StatusOK, "")
	var leaders []map[string]any
	if err := json.Unmarshal(got.body, &leaders); err != nil {
		t.Fatal(err)
	}
	if len(leaders) != 2 || leaders[0]["id"] != e.people["webLeader"].String() || leaders[0]["kind"] != "LIDERLER" ||
		leaders[1]["id"] != e.people["webCoord"].String() || leaders[1]["kind"] != "KOORDINATORLER" {
		t.Fatalf("leaders %s", got.body)
	}
	if _, ok := leaders[0]["username"]; ok {
		t.Errorf("a Leader read a username: %s", got.body)
	}
	if _, ok := leaders[0]["skyNumber"]; ok {
		t.Errorf("a Leader read a Sky number: %s", got.body)
	}
	wantStatus(t, "a member", e.send(e.token("member"), fiber.MethodGet, "/v1/groups/"+e.ids["/UYELER/ARGE/WEBLAB"]+"/leaders", ""), fiber.StatusForbidden, "")
	wantStatus(t, "a sibling Team's Leader", e.send(e.token("mobLeader"), fiber.MethodGet, "/v1/groups/"+e.ids["/UYELER/ARGE/WEBLAB"]+"/leaders", ""), fiber.StatusForbidden, "")
	full := e.send(e.token("yk"), fiber.MethodGet, "/v1/groups/"+e.ids["/UYELER/ARGE/WEBLAB/FRONT"]+"/leaders", "")
	wantStatus(t, "YK", full, fiber.StatusOK, "")
	if err := json.Unmarshal(full.body, &leaders); err != nil || len(leaders) != 1 || leaders[0]["username"] != "u-frontLeader" {
		t.Fatalf("YK's leaders %s", full.body)
	}
	empty := e.send(e.token("yk"), fiber.MethodGet, "/v1/groups/"+e.ids["/UYELER/ARGE/WEBLAB/SECURE"]+"/leaders", "")
	if empty.status != fiber.StatusOK || string(empty.body) != "[]" {
		t.Errorf("no Leaders: %d %s", empty.status, empty.body)
	}
}

// Whoever manages a Team strictly removes its Leaders; the answer says when
// none is left (the Team stays managed from above). Nobody leaves the
// Leaders of their own Team this way.
func TestTeamLeaderRemove(t *testing.T) {
	t.Parallel()
	e := newDelegationEnv(t, delegationSetup{on: true})
	before, heard := len(e.audit.Rows()), len(e.heard.all())
	wantStatus(t, "the Sub-team's own Leader leaves", e.removeLeader("frontLeader", "/UYELER/ARGE/WEBLAB/FRONT", "frontLeader"), fiber.StatusForbidden, "")
	wantStatus(t, "a top Team's Leader leaves", e.removeLeader("webLeader", "/UYELER/ARGE/WEBLAB", "webLeader"), fiber.StatusForbidden, "")
	wantStatus(t, "a top Team's Leader removes a co-Leader", e.removeLeader("webLeader", "/UYELER/ARGE/WEBLAB", "webCoord"), fiber.StatusForbidden, "")
	wantStatus(t, "a sibling Team's Leader", e.removeLeader("mobLeader", "/UYELER/ARGE/WEBLAB/FRONT", "frontLeader"), fiber.StatusForbidden, "")
	if !e.directlyIn("frontLeader", "/UYELER/ARGE/WEBLAB/FRONT/LIDERLER") || !e.directlyIn("webCoord", "/UYELER/ARGE/WEBLAB/KOORDINATORLER") {
		t.Fatal("a refused removal removed")
	}
	for _, row := range e.rowsSince(before) {
		if row.Outcome != teamaudit.OutcomeDenied {
			t.Errorf("a refusal wrote %+v", row)
		}
	}

	wantStatus(t, "a second Leader", e.addLeader("webLeader", "/UYELER/ARGE/WEBLAB/FRONT", "newcomer", "KOORDINATORLER"), fiber.StatusNoContent, "")
	before = len(e.audit.Rows())
	got := e.removeLeader("webLeader", "/UYELER/ARGE/WEBLAB/FRONT", "frontLeader")
	wantStatus(t, "one of two", got, fiber.StatusOK, "")
	if string(got.body) != `{"lastLeader":false}` {
		t.Errorf("answer %s", got.body)
	}
	rows := e.rowsSince(before)
	if len(rows) != 1 || rows[0].Action != teamaudit.ActionLeaderRemove || rows[0].Before == nil || rows[0].Before.Kind != "LIDERLER" ||
		rows[0].GroupID != e.ids["/UYELER/ARGE/WEBLAB/FRONT"] || rows[0].Outcome != teamaudit.OutcomeOK {
		t.Errorf("rows %+v, want one leader.remove row", rows)
	}
	if e.directlyIn("frontLeader", "/UYELER/ARGE/WEBLAB/FRONT/LIDERLER") {
		t.Error("still a Leader")
	}
	got = e.removeLeader("webLeader", "/UYELER/ARGE/WEBLAB/FRONT", "newcomer")
	wantStatus(t, "the last one", got, fiber.StatusOK, "")
	if string(got.body) != `{"lastLeader":true}` {
		t.Errorf("answer %s", got.body)
	}
	changes := e.heard.all()[heard:]
	removed := 0
	for _, c := range changes {
		if !c.Added {
			removed++
		}
	}
	if removed != 2 {
		t.Errorf("heard %+v", changes)
	}
	wantStatus(t, "no Leader of it", e.removeLeader("webLeader", "/UYELER/ARGE/WEBLAB/FRONT", "newcomer"), fiber.StatusNotFound, "")
	// Still managed from above: WEBLAB's Leader names a new one.
	wantStatus(t, "a new Leader", e.addLeader("webLeader", "/UYELER/ARGE/WEBLAB/FRONT", "frontMember", ""), fiber.StatusNoContent, "")
	wantStatus(t, "YK on a top Team", e.removeLeader("yk", "/UYELER/ARGE/WEBLAB", "webCoord"), fiber.StatusOK, "")
}

// #227 review Y1 and S2: with delegation on a Leader group is made only by
// naming a Leader, Privileged's too; no Group sits under one; and no route
// lets a Leader name a co-Leader of their own Team.
func TestLeaderGroupsOnlyThroughTheLeadersRoute(t *testing.T) {
	t.Parallel()
	e := newDelegationEnv(t, delegationSetup{on: true})
	yk := e.token("yk")
	wantStatus(t, "YK opens KOORDINATORLER by hand", e.send(yk, fiber.MethodPost, "/v1/groups",
		`{"name":"KOORDINATORLER","parentId":"`+e.ids["/UYELER/ARGE/MOBILAB"]+`"}`), fiber.StatusUnprocessableEntity, teamcode.ReservedName)
	wantStatus(t, "YK renames a Sub-team to LIDERLER", e.send(yk, fiber.MethodPatch, "/v1/groups/"+e.ids["/UYELER/ARGE/WEBLAB/SECURE"],
		`{"name":"KOORDINATORLER"}`), fiber.StatusUnprocessableEntity, teamcode.ReservedName)
	wantStatus(t, "YK under a Leader group", e.send(yk, fiber.MethodPost, "/v1/groups",
		`{"name":"UNDERLAB","parentId":"`+e.ids["/UYELER/ARGE/WEBLAB/LIDERLER"]+`"}`), fiber.StatusBadRequest, "")
	wantStatus(t, "a Sub-team under a Leader group", e.createSub("webLeader", "/UYELER/ARGE/WEBLAB/LIDERLER", "UNDERLAB"), fiber.StatusBadRequest, "")
	// WEBLAB's Leader cannot make a co-Coordinator of WEBLAB by any route.
	wantStatus(t, "a Sub-team named KOORDINATORLER", e.createSub("webLeader", "/UYELER/ARGE/WEBLAB", "KOORDINATORLER"), fiber.StatusUnprocessableEntity, teamcode.ReservedName)
	wantStatus(t, "a Sub-team renamed KOORDINATORLER", e.changeCode("webLeader", "/UYELER/ARGE/WEBLAB/FRONT", "KOORDINATORLER"), fiber.StatusUnprocessableEntity, teamcode.ReservedName)
	wantStatus(t, "a Coordinator of their own Team", e.addLeader("webLeader", "/UYELER/ARGE/WEBLAB", "frontMember", "KOORDINATORLER"), fiber.StatusForbidden, "")
	if e.idOf("/UYELER/ARGE/MOBILAB/KOORDINATORLER") != "" || e.idOf("/UYELER/ARGE/WEBLAB/LIDERLER/UNDERLAB") != "" {
		t.Error("a Leader group or a Group under one was made by hand")
	}

	off := newDelegationEnv(t, delegationSetup{})
	wantStatus(t, "off, YK opens one by hand as before", off.send(off.token("yk"), fiber.MethodPost, "/v1/groups",
		`{"name":"KOORDINATORLER","parentId":"`+off.ids["/UYELER/ARGE/MOBILAB"]+`"}`), fiber.StatusCreated, "")
	wantStatus(t, "off, a Leader is refused", off.addLeader("webLeader", "/UYELER/ARGE/WEBLAB/FRONT", "newcomer", ""), fiber.StatusForbidden, "")
	wantStatus(t, "off, YK names a Leader", off.addLeader("yk", "/UYELER/ARGE/WEBLAB/FRONT", "newcomer", ""), fiber.StatusNoContent, "")
}

// A Sub-team's Leader group that maps an admin role is Privileged's: a
// Leader neither adds nor removes a Leader there.
func TestTeamLeaderProtectedLeaderGroup(t *testing.T) {
	t.Parallel()
	e := newDelegationEnv(t, delegationSetup{on: true})
	if err := e.dir.SetGroupClientRoles(context.Background(), e.ids["/UYELER/ARGE/WEBLAB/FRONT/LIDERLER"],
		[]identity.ClientRole{{ClientID: "core", Role: authz.RoleEventManage}}); err != nil {
		t.Fatal(err)
	}
	wantStatus(t, "add", e.addLeader("webLeader", "/UYELER/ARGE/WEBLAB/FRONT", "newcomer", ""), fiber.StatusForbidden, "protected_group")
	wantStatus(t, "remove", e.removeLeader("webLeader", "/UYELER/ARGE/WEBLAB/FRONT", "frontLeader"), fiber.StatusForbidden, "protected_group")
	if e.directlyIn("newcomer", "/UYELER/ARGE/WEBLAB/FRONT/LIDERLER") || !e.directlyIn("frontLeader", "/UYELER/ARGE/WEBLAB/FRONT/LIDERLER") {
		t.Error("a protected Leader group changed")
	}
	wantStatus(t, "YK", e.addLeader("yk", "/UYELER/ARGE/WEBLAB/FRONT", "newcomer", ""), fiber.StatusNoContent, "")
}
