package httpx_test

import (
	"context"
	"encoding/json"
	"slices"
	"sync"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/identity"
	"github.com/skylab-kulubu/core-backend/internal/teamaudit"
	"github.com/skylab-kulubu/core-backend/internal/teamcode"
)

// idOf is the id of the Group at path, as the directory has it now (a
// Group a test created has no entry in e.ids); "" for none.
func (e *delegationEnv) idOf(path string) string {
	e.t.Helper()
	groups, err := e.dir.ListGroups(context.Background())
	if err != nil {
		e.t.Fatal(err)
	}
	for _, g := range groups {
		if g.Path == path {
			return g.ID
		}
	}
	return ""
}

func (e *delegationEnv) createSub(as, parent, code string) rawResponse {
	return e.send(e.token(as), fiber.MethodPost, "/v1/groups/"+e.idOf(parent)+"/children",
		`{"code":"`+code+`","displayNameTr":"Ekip `+code+`","displayNameEn":"Team `+code+`"}`)
}

func (e *delegationEnv) changeCode(as, path, code string) rawResponse {
	return e.send(e.token(as), fiber.MethodPut, "/v1/groups/"+e.idOf(path)+"/code", `{"code":"`+code+`"}`)
}

func (e *delegationEnv) deleteTeam(as, path string) rawResponse {
	return e.send(e.token(as), fiber.MethodDelete, "/v1/groups/"+e.idOf(path), "")
}

// put puts an empty Group at path into the directory, as Keycloak's
// console would.
func (e *delegationEnv) put(path string) string {
	id := uuid.NewString()
	e.dir.PutGroup(identity.Group{ID: id, Name: path[lastSlash(path)+1:], Path: path})
	return id
}

func lastSlash(path string) int {
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '/' {
			return i
		}
	}
	return -1
}

// rowsSince are the audit rows written since n.
func (e *delegationEnv) rowsSince(n int) []teamaudit.Row {
	return e.audit.Rows()[n:]
}

func (e *delegationEnv) retire(code string) {
	e.t.Helper()
	if err := e.codes.Locked(context.Background(), func(ctx context.Context, records teamcode.Records) error {
		return records.Retire(ctx, code, "", "seeded")
	}); err != nil {
		e.t.Fatal(err)
	}
}

// A Team's manager opens a Sub-team under it with its display names; a
// Sub-team's Leader under their own; Privileged anywhere. The create is a
// team.create row naming the Team that grants it.
func TestSubTeamCreate(t *testing.T) {
	t.Parallel()
	e := newDelegationEnv(t, delegationSetup{on: true})

	before := len(e.audit.Rows())
	got := e.createSub("webLeader", "/UYELER/ARGE/WEBLAB", "BACK")
	wantStatus(t, "the Team's Leader", got, fiber.StatusCreated, "")
	var created identity.Group
	if err := json.Unmarshal(got.body, &created); err != nil {
		t.Fatal(err)
	}
	if created.Path != "/UYELER/ARGE/WEBLAB/BACK" || created.Attributes["display_name_tr"] != "Ekip BACK" || created.Attributes["display_name_en"] != "Team BACK" {
		t.Errorf("created %+v", created)
	}
	rows := e.rowsSince(before)
	if len(rows) != 1 || rows[0].Action != teamaudit.ActionTeamCreate || rows[0].Outcome != teamaudit.OutcomeOK ||
		rows[0].Actor.Basis != teamaudit.BasisLeader || rows[0].Actor.ViaGroupID != e.ids["/UYELER/ARGE/WEBLAB"] ||
		rows[0].GroupID != created.ID || rows[0].After == nil || rows[0].After.Code != "BACK" ||
		rows[0].After.Attributes["display_name_tr"] != "Ekip BACK" {
		t.Errorf("rows %+v, want one team.create row by WEBLAB's Leader", rows)
	}

	wantStatus(t, "a Coordinator", e.createSub("webCoord", "/UYELER/ARGE/WEBLAB", "COORDLAB"), fiber.StatusCreated, "")
	wantStatus(t, "a Sub-team's Leader under their own", e.createSub("frontLeader", "/UYELER/ARGE/WEBLAB/FRONT", "FRONT-UI"), fiber.StatusCreated, "")
	wantStatus(t, "a Leader of the Team above, under a Sub-team", e.createSub("webLeader", "/UYELER/ARGE/WEBLAB/FRONT", "FRONT-API"), fiber.StatusCreated, "")
	before = len(e.audit.Rows())
	wantStatus(t, "YK", e.createSub("yk", "/UYELER/ARGE/MOBILAB", "MOBIL-IOS"), fiber.StatusCreated, "")
	if rows := e.rowsSince(before); len(rows) != 1 || rows[0].Actor.Basis != teamaudit.BasisPrivileged {
		t.Errorf("YK's rows %+v", rows)
	}

	before = len(e.audit.Rows())
	for _, c := range []struct{ name, as, parent, code string }{
		{"a Sub-team's Leader on the Team above", "frontLeader", "/UYELER/ARGE/WEBLAB", "UPLAB"},
		{"a sibling Team's Leader", "mobLeader", "/UYELER/ARGE/WEBLAB", "SIBLAB"},
		{"a member", "member", "/UYELER/ARGE/WEBLAB", "MEMBLAB"},
		{"an outsider", "outsider", "/UYELER/ARGE/WEBLAB", "OUTLAB"},
	} {
		wantStatus(t, c.name, e.createSub(c.as, c.parent, c.code), fiber.StatusForbidden, "")
		if e.idOf(c.parent+"/"+c.code) != "" {
			t.Errorf("%s: created", c.name)
		}
	}
	wantStatus(t, "under a protected Team", e.createSub("webLeader", "/UYELER/ARGE/WEBLAB/SECURE", "SECLAB"), fiber.StatusForbidden, "protected_group")
	for _, row := range e.rowsSince(before) {
		if row.Outcome != teamaudit.OutcomeDenied || row.Action != teamaudit.ActionTeamCreate {
			t.Errorf("a refusal wrote %+v", row)
		}
	}
	if n := len(e.rowsSince(before)); n != 5 {
		t.Errorf("%d denied rows, want 5", n)
	}

	wantStatus(t, "under a Leader group", e.createSub("webLeader", "/UYELER/ARGE/WEBLAB/LIDERLER", "LEADLAB"), fiber.StatusBadRequest, "")
	wantStatus(t, "YK under a Leader group", e.createSub("yk", "/UYELER/ARGE/WEBLAB/LIDERLER", "LEADLAB"), fiber.StatusBadRequest, "")
	wantStatus(t, "YK under an area", e.createSub("yk", "/UYELER/ARGE", "AREALAB"), fiber.StatusBadRequest, "")
	wantStatus(t, "no Turkish name", e.send(e.token("webLeader"), fiber.MethodPost, "/v1/groups/"+e.ids["/UYELER/ARGE/WEBLAB"]+"/children", `{"code":"NONAME"}`),
		fiber.StatusBadRequest, "")
	wantStatus(t, "YK, a parent by path", e.send(e.token("yk"), fiber.MethodPost, "/v1/groups/%2FUYELER%2FARGE%2FWEBLAB/children", `{"code":"PATHLAB","displayNameTr":"P"}`),
		fiber.StatusNotFound, "")
	wantStatus(t, "a Leader, a parent by path", e.send(e.token("webLeader"), fiber.MethodPost, "/v1/groups/%2FUYELER%2FARGE%2FWEBLAB/children", `{"code":"PATHLAB","displayNameTr":"P"}`),
		fiber.StatusForbidden, "")
}

// The names a Sub-team never gets (#227 review Y1 and Y2): a Leader
// group's, one that looks like it, a reserved or malformed one, one another
// Group, a record of core or a retired code holds. YK is refused the same.
func TestSubTeamCreateNames(t *testing.T) {
	t.Parallel()
	e := newDelegationEnv(t, delegationSetup{on: true})
	e.codes.Hold("EVENTLAB")
	e.codes.HoldAs(teamcode.RecordTemplateBinding, "BOUNDLAB")
	// A code only the CMS, Forms or a site names, written to the retired
	// codes by ticket 03's report (--seed-retired).
	e.retire("CMSLAB")
	for _, as := range []string{"webLeader", "yk"} {
		for code, want := range map[string]struct {
			status int
			code   string
		}{
			"LIDERLER":        {fiber.StatusUnprocessableEntity, teamcode.ReservedName},
			"KOORDINATORLER":  {fiber.StatusUnprocessableEntity, teamcode.ReservedName},
			"LIDERLER-ESKI":   {fiber.StatusUnprocessableEntity, teamcode.ReservedName},
			"KOORDINATORLER2": {fiber.StatusUnprocessableEntity, teamcode.ReservedName},
			"ADMIN":           {fiber.StatusUnprocessableEntity, teamcode.ReservedName},
			"GECEKODU":        {fiber.StatusUnprocessableEntity, teamcode.ReservedName},
			"admin":           {fiber.StatusUnprocessableEntity, teamcode.InvalidName},
			"WEB/LAB":         {fiber.StatusUnprocessableEntity, teamcode.InvalidName},
			"MOBILAB":         {fiber.StatusConflict, teamcode.NameTaken},
			"mobilab":         {fiber.StatusUnprocessableEntity, teamcode.InvalidName},
			"FRONT":           {fiber.StatusConflict, teamcode.NameTaken},
			"EVENTLAB":        {fiber.StatusConflict, teamcode.NameTaken},
			"BOUNDLAB":        {fiber.StatusConflict, teamcode.NameTaken},
			"CMSLAB":          {fiber.StatusConflict, teamcode.NameTaken},
		} {
			groups, _ := e.dir.ListGroups(context.Background())
			wantStatus(t, as+" "+code, e.createSub(as, "/UYELER/ARGE/WEBLAB", code), want.status, want.code)
			if after, _ := e.dir.ListGroups(context.Background()); len(after) != len(groups) {
				t.Errorf("%s %s: created", as, code)
			}
		}
	}
}

// The Team structure limits (SPEC §4.5) hold for every caller: two levels
// of Sub-teams below a top Team, a path's length, a Team's Sub-teams.
func TestSubTeamLimits(t *testing.T) {
	t.Parallel()
	e := newDelegationEnv(t, delegationSetup{on: true})
	wantStatus(t, "two levels down", e.createSub("webLeader", "/UYELER/ARGE/WEBLAB/FRONT", "X1"), fiber.StatusCreated, "")
	wantStatus(t, "three levels down", e.createSub("webLeader", "/UYELER/ARGE/WEBLAB/FRONT/X1", "X2"), fiber.StatusConflict, "depth_limit")
	wantStatus(t, "YK, three levels down", e.createSub("yk", "/UYELER/ARGE/WEBLAB/FRONT/X1", "X2"), fiber.StatusConflict, "depth_limit")

	small := newDelegationEnv(t, delegationSetup{on: true, subTeams: 2, pathLen: 30})
	// WEBLAB has FRONT and SECURE; its Leader groups do not count.
	wantStatus(t, "a third Sub-team", small.createSub("webLeader", "/UYELER/ARGE/WEBLAB", "THIRD"), fiber.StatusConflict, "children_limit")
	wantStatus(t, "YK, a third Sub-team", small.createSub("yk", "/UYELER/ARGE/WEBLAB", "THIRD"), fiber.StatusConflict, "children_limit")
	// /UYELER/ARGE/MOBILAB/ is 21 characters.
	wantStatus(t, "a path of 30", small.createSub("mobLeader", "/UYELER/ARGE/MOBILAB", "ABCDEFGHI"), fiber.StatusCreated, "")
	wantStatus(t, "a path of 31", small.createSub("mobLeader", "/UYELER/ARGE/MOBILAB", "ABCDEFGHIJ"), fiber.StatusConflict, "depth_limit")
}

// With TEAM_DELEGATION_LEADER_CODES on, whoever manages a Sub-team strictly
// changes its code: the old one is retired, the Groups under it move with
// it, the row is team.code. Its own Leader does not, nor anyone on a top
// Team but Privileged.
func TestTeamCodeChange(t *testing.T) {
	t.Parallel()
	e := newDelegationEnv(t, delegationSetup{on: true, teamTree: true, leaderCodes: true})
	before, forgets := len(e.audit.Rows()), e.tree.forgotten()
	got := e.changeCode("webLeader", "/UYELER/ARGE/WEBLAB/FRONT", "FRONTEND")
	wantStatus(t, "a Leader of the Team above", got, fiber.StatusOK, "")
	if e.idOf("/UYELER/ARGE/WEBLAB/FRONTEND") != e.ids["/UYELER/ARGE/WEBLAB/FRONT"] || e.idOf("/UYELER/ARGE/WEBLAB/FRONTEND/LIDERLER") == "" {
		t.Error("the Sub-team or its Leader group did not move to the new path")
	}
	if e.codes.Retired()["FRONT"] != teamcode.ReasonRenamed {
		t.Errorf("retired %v, want FRONT renamed", e.codes.Retired())
	}
	if e.tree.forgotten() == forgets {
		t.Error("the change did not forget the kept Group reads")
	}
	rows := e.rowsSince(before)
	if len(rows) != 1 || rows[0].Action != teamaudit.ActionTeamCode || rows[0].Outcome != teamaudit.OutcomeOK ||
		rows[0].Actor.Basis != teamaudit.BasisLeader || rows[0].Before.Code != "FRONT" || rows[0].After.Code != "FRONTEND" {
		t.Errorf("rows %+v, want one team.code row", rows)
	}
	// Its Leader still leads it, under the new path.
	wantStatus(t, "the Sub-team's Leader adds after the change", e.send(e.token("frontLeader"), fiber.MethodPost,
		"/v1/groups/"+e.idOf("/UYELER/ARGE/WEBLAB/FRONTEND")+"/members", `{"userId":"`+e.people["newcomer"].String()+`"}`), fiber.StatusNoContent, "")
	wantStatus(t, "the retired code", e.createSub("yk", "/UYELER/ARGE/MOBILAB", "FRONT"), fiber.StatusConflict, teamcode.NameTaken)

	before = len(e.audit.Rows())
	wantStatus(t, "the Sub-team's own Leader", e.changeCode("frontLeader", "/UYELER/ARGE/WEBLAB/FRONTEND", "FRONTX"), fiber.StatusForbidden, "")
	wantStatus(t, "a top Team's Leader on it", e.changeCode("webLeader", "/UYELER/ARGE/WEBLAB", "WEBX"), fiber.StatusForbidden, "")
	wantStatus(t, "a sibling Team's Leader", e.changeCode("mobLeader", "/UYELER/ARGE/WEBLAB/FRONTEND", "FRONTX"), fiber.StatusForbidden, "")
	wantStatus(t, "a member", e.changeCode("frontMember", "/UYELER/ARGE/WEBLAB/FRONTEND", "FRONTX"), fiber.StatusForbidden, "")
	wantStatus(t, "a protected Sub-team", e.changeCode("webLeader", "/UYELER/ARGE/WEBLAB/SECURE", "SECX"), fiber.StatusForbidden, "protected_group")
	wantStatus(t, "a Leader group", e.changeCode("webLeader", "/UYELER/ARGE/WEBLAB/FRONTEND/LIDERLER", "LEADX"), fiber.StatusForbidden, "")
	for _, row := range e.rowsSince(before) {
		if row.Outcome != teamaudit.OutcomeDenied {
			t.Errorf("a refusal wrote %+v", row)
		}
	}
	if e.idOf("/UYELER/ARGE/WEBLAB/FRONTEND") == "" || e.idOf("/UYELER/ARGE/WEBLAB") == "" {
		t.Error("a refused change renamed")
	}
	wantStatus(t, "YK on a top Team", e.changeCode("yk", "/UYELER/ARGE/MOBILAB", "MOBILE"), fiber.StatusOK, "")
	wantStatus(t, "YK on a Leader group", e.changeCode("yk", "/UYELER/ARGE/MOBILE/LIDERLER", "LEADX"), fiber.StatusBadRequest, "")
	wantStatus(t, "YK on an area", e.changeCode("yk", "/UYELER/ARGE", "ARGEX"), fiber.StatusBadRequest, "")

	before = len(e.audit.Rows())
	wantStatus(t, "the same code", e.changeCode("webLeader", "/UYELER/ARGE/WEBLAB/FRONTEND", "FRONTEND"), fiber.StatusOK, "")
	if rows := e.rowsSince(before); len(rows) != 0 {
		t.Errorf("the same code wrote %+v", rows)
	}
}

// A code core's records name is not changed (S9): the answer names the
// kinds of record, never one. Nothing is retired or renamed.
func TestTeamCodeInUse(t *testing.T) {
	t.Parallel()
	e := newDelegationEnv(t, delegationSetup{on: true, leaderCodes: true})
	e.codes.HoldAs(teamcode.RecordCertificate, "front")
	e.codes.Hold("FRONT")
	for _, as := range []string{"webLeader", "yk"} {
		got := e.changeCode(as, "/UYELER/ARGE/WEBLAB/FRONT", "FRONTEND")
		wantStatus(t, as, got, fiber.StatusConflict, "code_in_use")
		records, _ := got.problem()["records"].([]any)
		if len(records) != 2 || records[0] != teamcode.RecordEvent || records[1] != teamcode.RecordCertificate {
			t.Errorf("%s: records %v", as, got.problem()["records"])
		}
	}
	if e.idOf("/UYELER/ARGE/WEBLAB/FRONT") == "" || len(e.codes.Retired()) != 0 {
		t.Errorf("a refused change renamed or retired: %v", e.codes.Retired())
	}
}

// A Sub-team never takes a Leader group's name, nor one that looks like it,
// nor a taken one (#227 review Y1: a Sub-team made KOORDINATORLER would
// make its members Leaders of the Team above).
func TestTeamCodeChangeNames(t *testing.T) {
	t.Parallel()
	e := newDelegationEnv(t, delegationSetup{on: true, leaderCodes: true})
	e.retire("CMSLAB")
	for _, as := range []string{"webLeader", "yk"} {
		for code, want := range map[string]struct {
			status int
			code   string
		}{
			"KOORDINATORLER": {fiber.StatusUnprocessableEntity, teamcode.ReservedName},
			"LIDERLER":       {fiber.StatusUnprocessableEntity, teamcode.ReservedName},
			"LIDERLER-YENI":  {fiber.StatusUnprocessableEntity, teamcode.ReservedName},
			"YK":             {fiber.StatusUnprocessableEntity, teamcode.ReservedName},
			"front-end":      {fiber.StatusUnprocessableEntity, teamcode.InvalidName},
			"MOBILAB":        {fiber.StatusConflict, teamcode.NameTaken},
			"CMSLAB":         {fiber.StatusConflict, teamcode.NameTaken},
		} {
			wantStatus(t, as+" "+code, e.changeCode(as, "/UYELER/ARGE/WEBLAB/FRONT", code), want.status, want.code)
		}
	}
	if e.idOf("/UYELER/ARGE/WEBLAB/FRONT") != e.ids["/UYELER/ARGE/WEBLAB/FRONT"] || len(e.codes.Retired()) != 1 {
		t.Errorf("a refused change renamed or retired: %v", e.codes.Retired())
	}
}

// Whoever manages a Sub-team strictly deletes it, with every Sub-team and
// Leader group under it, once nobody is in any of them and core's records
// name none of their codes (S10). Every code given up is retired.
func TestTeamDelete(t *testing.T) {
	t.Parallel()
	e := newDelegationEnv(t, delegationSetup{on: true})
	wantStatus(t, "open", e.createSub("webLeader", "/UYELER/ARGE/WEBLAB", "EMPTY"), fiber.StatusCreated, "")
	wantStatus(t, "open below", e.createSub("webLeader", "/UYELER/ARGE/WEBLAB/EMPTY", "EMPTY-SUB"), fiber.StatusCreated, "")
	e.put("/UYELER/ARGE/WEBLAB/EMPTY/LIDERLER")

	before := len(e.audit.Rows())
	wantStatus(t, "the Sub-team's own Leader", e.deleteTeam("frontLeader", "/UYELER/ARGE/WEBLAB/FRONT"), fiber.StatusForbidden, "")
	wantStatus(t, "a top Team's Leader on it", e.deleteTeam("webLeader", "/UYELER/ARGE/WEBLAB"), fiber.StatusForbidden, "")
	wantStatus(t, "a sibling Team's Leader", e.deleteTeam("mobLeader", "/UYELER/ARGE/WEBLAB/EMPTY"), fiber.StatusForbidden, "")
	wantStatus(t, "a member", e.deleteTeam("member", "/UYELER/ARGE/WEBLAB/EMPTY"), fiber.StatusForbidden, "")
	wantStatus(t, "a Leader group", e.deleteTeam("webLeader", "/UYELER/ARGE/WEBLAB/EMPTY/LIDERLER"), fiber.StatusForbidden, "")
	wantStatus(t, "a protected Sub-team", e.deleteTeam("webLeader", "/UYELER/ARGE/WEBLAB/SECURE"), fiber.StatusForbidden, "protected_group")
	for _, row := range e.rowsSince(before) {
		if row.Outcome != teamaudit.OutcomeDenied {
			t.Errorf("a refusal wrote %+v", row)
		}
	}

	got := e.deleteTeam("webLeader", "/UYELER/ARGE/WEBLAB/FRONT")
	wantStatus(t, "people in it and in its Leader group", got, fiber.StatusConflict, "not_empty")
	if p := got.problem(); p["members"] != float64(2) || p["children"] != float64(1) {
		t.Errorf("counts %v", p)
	}

	before = len(e.audit.Rows())
	wantStatus(t, "an empty Sub-team", e.deleteTeam("webLeader", "/UYELER/ARGE/WEBLAB/EMPTY"), fiber.StatusNoContent, "")
	for _, path := range []string{"/UYELER/ARGE/WEBLAB/EMPTY", "/UYELER/ARGE/WEBLAB/EMPTY/EMPTY-SUB", "/UYELER/ARGE/WEBLAB/EMPTY/LIDERLER"} {
		if e.idOf(path) != "" {
			t.Errorf("%s remains", path)
		}
	}
	if r := e.codes.Retired(); r["EMPTY"] != teamcode.ReasonDeleted || r["EMPTY-SUB"] != teamcode.ReasonDeleted || r["LIDERLER"] != "" {
		t.Errorf("retired %v", r)
	}
	rows := e.rowsSince(before)
	if len(rows) != 1 || rows[0].Action != teamaudit.ActionTeamDelete || rows[0].Outcome != teamaudit.OutcomeOK ||
		rows[0].Actor.Basis != teamaudit.BasisLeader || rows[0].Actor.ViaGroupID != e.ids["/UYELER/ARGE/WEBLAB"] {
		t.Errorf("rows %+v, want one team.delete row by WEBLAB's Leader", rows)
	}
	wantStatus(t, "a deleted code again", e.createSub("webLeader", "/UYELER/ARGE/WEBLAB", "EMPTY-SUB"), fiber.StatusConflict, teamcode.NameTaken)

	// A top Team is Privileged's, with the same guards.
	wantStatus(t, "YK opens a top Team", e.send(e.token("yk"), fiber.MethodPost, "/v1/groups",
		`{"name":"TOPLAB","parentId":"`+e.ids["/UYELER/ORGANIZASYON"]+`"}`), fiber.StatusCreated, "")
	e.put("/UYELER/ORGANIZASYON/TOPLAB/LIDERLER")
	wantStatus(t, "YK deletes a top Team", e.deleteTeam("yk", "/UYELER/ORGANIZASYON/TOPLAB"), fiber.StatusNoContent, "")
	wantStatus(t, "YK on a Team with Leaders", e.deleteTeam("yk", "/UYELER/ARGE/MOBILAB"), fiber.StatusConflict, "not_empty")
	wantStatus(t, "YK on its root", e.deleteTeam("yk", "/UYELER/YK"), fiber.StatusConflict, teamcode.PrivilegedRoot)
}

// A Team whose subtree holds a code core's records name, a role mapping or
// a Group that is no Sub-team or Leader group is not deleted.
func TestTeamDeleteSubtreeGuards(t *testing.T) {
	t.Parallel()
	e := newDelegationEnv(t, delegationSetup{on: true})
	for _, code := range []string{"OUTER", "ROLED", "ODD"} {
		wantStatus(t, "open "+code, e.createSub("webLeader", "/UYELER/ARGE/WEBLAB", code), fiber.StatusCreated, "")
	}
	wantStatus(t, "open INNER", e.createSub("webLeader", "/UYELER/ARGE/WEBLAB/OUTER", "INNER"), fiber.StatusCreated, "")
	e.codes.HoldAs(teamcode.RecordCertificateTemplate, "INNER")
	got := e.deleteTeam("webLeader", "/UYELER/ARGE/WEBLAB/OUTER")
	wantStatus(t, "a code below in use", got, fiber.StatusConflict, "code_in_use")
	if records, _ := got.problem()["records"].([]any); len(records) != 1 || records[0] != teamcode.RecordCertificateTemplate {
		t.Errorf("records %v", got.problem()["records"])
	}

	leaders := e.put("/UYELER/ARGE/WEBLAB/ROLED/LIDERLER")
	if err := e.dir.SetGroupClientRoles(context.Background(), leaders, []identity.ClientRole{{ClientID: "forms", Role: "skyforms:access"}}); err != nil {
		t.Fatal(err)
	}
	wantStatus(t, "a role mapping below", e.deleteTeam("webLeader", "/UYELER/ARGE/WEBLAB/ROLED"), fiber.StatusForbidden, "group_delete_protected")

	e.put("/UYELER/ARGE/WEBLAB/ODD/LIDERLER")
	e.put("/UYELER/ARGE/WEBLAB/ODD/LIDERLER/X")
	wantStatus(t, "a Group under a Leader group", e.deleteTeam("webLeader", "/UYELER/ARGE/WEBLAB/ODD"), fiber.StatusForbidden, "group_delete_protected")

	if len(e.codes.Retired()) != 0 {
		t.Errorf("a refused deletion retired %v", e.codes.Retired())
	}
	for _, path := range []string{"/UYELER/ARGE/WEBLAB/OUTER/INNER", "/UYELER/ARGE/WEBLAB/ROLED/LIDERLER", "/UYELER/ARGE/WEBLAB/ODD/LIDERLER/X"} {
		if e.idOf(path) == "" {
			t.Errorf("%s was deleted", path)
		}
	}
}

// By default (TEAM_DELEGATION_LEADER_CODES off) only Privileged changes a
// Team's code (Yusuf, 2026-10-31): a Leader who manages the Sub-team
// strictly is refused 403 team_code_privileged_only with a denied team.code
// row naming the Team they acted for, nothing renamed or retired; YK changes
// the same Sub-team's code. Leaders still open and delete Sub-teams.
func TestTeamCodeChangePrivilegedOnly(t *testing.T) {
	t.Parallel()
	e := newDelegationEnv(t, delegationSetup{on: true})
	before := len(e.audit.Rows())
	wantStatus(t, "a Leader of the Team above", e.changeCode("webLeader", "/UYELER/ARGE/WEBLAB/FRONT", "FRONTEND"),
		fiber.StatusForbidden, "team_code_privileged_only")
	rows := e.rowsSince(before)
	if len(rows) != 1 || rows[0].Action != teamaudit.ActionTeamCode || rows[0].Outcome != teamaudit.OutcomeDenied ||
		rows[0].ErrorCode != "team_code_privileged_only" || rows[0].Actor.Basis != teamaudit.BasisLeader ||
		rows[0].Actor.ViaGroupID != e.ids["/UYELER/ARGE/WEBLAB"] {
		t.Errorf("rows %+v, want one denied team.code row", rows)
	}
	if e.idOf("/UYELER/ARGE/WEBLAB/FRONT") != e.ids["/UYELER/ARGE/WEBLAB/FRONT"] || len(e.codes.Retired()) != 0 {
		t.Errorf("a refused change renamed or retired: %v", e.codes.Retired())
	}
	// Refusals that come first keep their own answers.
	wantStatus(t, "the Sub-team's own Leader", e.changeCode("frontLeader", "/UYELER/ARGE/WEBLAB/FRONT", "FRONTX"), fiber.StatusForbidden, "")
	wantStatus(t, "a protected Sub-team", e.changeCode("webLeader", "/UYELER/ARGE/WEBLAB/SECURE", "SECX"), fiber.StatusForbidden, "protected_group")
	wantStatus(t, "YK", e.changeCode("yk", "/UYELER/ARGE/WEBLAB/FRONT", "FRONTEND"), fiber.StatusOK, "")
	if e.idOf("/UYELER/ARGE/WEBLAB/FRONTEND") != e.ids["/UYELER/ARGE/WEBLAB/FRONT"] {
		t.Error("YK's change did not rename")
	}
	wantStatus(t, "a Leader still opens a Sub-team", e.createSub("webLeader", "/UYELER/ARGE/WEBLAB", "NEWLAB"), fiber.StatusCreated, "")
	wantStatus(t, "and deletes it", e.deleteTeam("webLeader", "/UYELER/ARGE/WEBLAB/NEWLAB"), fiber.StatusNoContent, "")
}

// With delegation off the new routes are Privileged's: a Leader is refused
// with nothing read or written.
func TestTeamStructureDelegationOff(t *testing.T) {
	t.Parallel()
	e := newDelegationEnv(t, delegationSetup{})
	ops := len(e.dir.Ops)
	wantStatus(t, "create", e.createSub("webLeader", "/UYELER/ARGE/WEBLAB", "OFFLAB"), fiber.StatusForbidden, "")
	wantStatus(t, "code", e.changeCode("webLeader", "/UYELER/ARGE/WEBLAB/FRONT", "OFFLAB"), fiber.StatusForbidden, "")
	wantStatus(t, "delete", e.deleteTeam("webLeader", "/UYELER/ARGE/WEBLAB/FRONT"), fiber.StatusForbidden, "")
	if rows := e.audit.Rows(); len(rows) != 0 {
		t.Errorf("rows %+v", rows)
	}
	for _, op := range e.dir.Ops[ops:] {
		if op != "ListGroups" && op != "GetGroup" && op != "GroupsForUser" {
			t.Errorf("a refused Leader read %s", op)
		}
	}
	wantStatus(t, "YK creates", e.createSub("yk", "/UYELER/ARGE/WEBLAB", "OFFLAB"), fiber.StatusCreated, "")
	wantStatus(t, "YK changes a code", e.changeCode("yk", "/UYELER/ARGE/WEBLAB/OFFLAB", "OFFLAB2"), fiber.StatusOK, "")
	wantStatus(t, "YK deletes", e.deleteTeam("yk", "/UYELER/ARGE/WEBLAB/OFFLAB2"), fiber.StatusNoContent, "")
}

// Two writes that would give one code, at once, through the lock: exactly
// one gets it, the other is refused as taken (a rename and a rename, a
// rename and a create).
func TestTeamCodeRaces(t *testing.T) {
	t.Parallel()
	for _, race := range []string{"rename-rename", "rename-create"} {
		t.Run(race, func(t *testing.T) {
			t.Parallel()
			e := newDelegationEnv(t, delegationSetup{on: true, leaderCodes: true})
			first := func() rawResponse { return e.changeCode("webLeader", "/UYELER/ARGE/WEBLAB/FRONT", "RACE") }
			second := func() rawResponse { return e.changeCode("yk", "/UYELER/ARGE/MOBILAB", "RACE") }
			if race == "rename-create" {
				second = func() rawResponse { return e.createSub("yk", "/UYELER/ARGE/MOBILAB", "RACE") }
			}
			var wg sync.WaitGroup
			got := make([]rawResponse, 2)
			for i, write := range []func() rawResponse{first, second} {
				wg.Add(1)
				go func() {
					defer wg.Done()
					got[i] = write()
				}()
			}
			wg.Wait()
			statuses := []int{got[0].status, got[1].status}
			slices.Sort(statuses)
			if statuses[1] != fiber.StatusConflict || (statuses[0] != fiber.StatusOK && statuses[0] != fiber.StatusCreated) {
				t.Fatalf("statuses %v (%s / %s), want one success and one 409", statuses, got[0].body, got[1].body)
			}
			groups, err := e.dir.ListGroups(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			n := 0
			for _, g := range groups {
				if g.Name == "RACE" {
					n++
				}
			}
			if n != 1 {
				t.Fatalf("%d Groups named RACE", n)
			}
		})
	}
}

// The managed tree says what the caller may do to each Team's structure:
// code.write for a Leader only with TEAM_DELEGATION_LEADER_CODES on.
func TestManagedTreeStructureCan(t *testing.T) {
	t.Parallel()
	for _, leaderCodes := range []bool{false, true} {
		e := newDelegationEnv(t, delegationSetup{on: true, leaderCodes: leaderCodes})
		got := e.send(e.token("webLeader"), fiber.MethodGet, "/v1/users/me/managed-groups", "")
		wantStatus(t, "tree", got, fiber.StatusOK, "")
		var nodes []identity.ManagedNode
		if err := json.Unmarshal(got.body, &nodes); err != nil {
			t.Fatal(err)
		}
		if len(nodes) != 1 {
			t.Fatalf("nodes %+v", nodes)
		}
		web := nodes[0]
		if !web.Can["subteams.write"] || web.Can["code.write"] || web.Can["team.delete"] {
			t.Errorf("leader codes %v: WEBLAB can %v", leaderCodes, web.Can)
		}
		i := slices.IndexFunc(web.Children, func(n identity.ManagedNode) bool { return n.Code == "FRONT" })
		if i < 0 || !web.Children[i].Can["subteams.write"] || web.Children[i].Can["code.write"] != leaderCodes || !web.Children[i].Can["team.delete"] {
			t.Errorf("leader codes %v: FRONT %+v", leaderCodes, web.Children)
		}
		j := slices.IndexFunc(web.Children, func(n identity.ManagedNode) bool { return n.Code == "SECURE" })
		if j < 0 || web.Children[j].Can["subteams.write"] || web.Children[j].Can["code.write"] || web.Children[j].Can["team.delete"] {
			t.Errorf("leader codes %v: SECURE %+v", leaderCodes, web.Children)
		}
	}
	// The Team settings (PUT /v1/groups/{id}/settings) are the manager's:
	// the Leader's own Team and those below, not a protected one.
	if !web.Can["team.settings"] || !web.Children[i].Can["team.settings"] || web.Children[j].Can["team.settings"] {
		t.Errorf("team.settings: WEBLAB %v, FRONT %v, SECURE %v", web.Can, web.Children[i].Can, web.Children[j].Can)
	}
}
