package httpx_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/skylab-kulubu/core-backend/internal/teamaudit"
)

func (e *delegationEnv) setting(as, path, body string) rawResponse {
	return e.send(e.token(as), fiber.MethodPut, e.group(path)+"/settings", body)
}

func (e *delegationEnv) attribute(path, name string) string {
	g, err := e.dir.GetGroup(context.Background(), e.ids[path])
	if err != nil {
		e.t.Fatal(err)
	}
	return g.Attributes[name]
}

// A Team's Leader (or one of a Team above) and Privileged choose whether
// the Team's members manage its competitors (Yusuf, B1 of 2026-10-25); the
// change is a group.attributes row with its old and new value, a refusal a
// denied row.
func TestTeamSettingsMembersManageCompetitors(t *testing.T) {
	t.Parallel()
	e := newDelegationEnv(t, delegationSetup{on: true})
	off := `{"membersManageCompetitors":false}`

	before := len(e.audit.Rows())
	got := e.setting("webLeader", "/UYELER/ARGE/WEBLAB", off)
	wantStatus(t, "the Team's Leader", got, fiber.StatusOK, "")
	if string(got.body) != off {
		t.Errorf("answer %s, want %s", got.body, off)
	}
	if v := e.attribute("/UYELER/ARGE/WEBLAB", "members_manage_competitors"); v != "false" {
		t.Errorf("attribute %q, want false", v)
	}
	rows := e.audit.Rows()[before:]
	if len(rows) != 1 || rows[0].Action != teamaudit.ActionGroupAttributes || rows[0].Outcome != teamaudit.OutcomeOK ||
		rows[0].After == nil || rows[0].After.Attributes["members_manage_competitors"] != "false" ||
		rows[0].Actor.Basis != teamaudit.BasisLeader {
		t.Errorf("rows %+v, want one group.attributes row by a Leader with the new value", rows)
	}

	wantStatus(t, "a Leader of the Team above on a Sub-team", e.setting("webLeader", "/UYELER/ARGE/WEBLAB/FRONT", off), fiber.StatusOK, "")
	wantStatus(t, "YK", e.setting("yk", "/UYELER/ARGE/MOBILAB", off), fiber.StatusOK, "")
	wantStatus(t, "back on", e.setting("yk", "/UYELER/ARGE/MOBILAB", `{"membersManageCompetitors":true}`), fiber.StatusOK, "")
	if v := e.attribute("/UYELER/ARGE/MOBILAB", "members_manage_competitors"); v != "true" {
		t.Errorf("MOBILAB attribute %q, want true", v)
	}

	before = len(e.audit.Rows())
	wantStatus(t, "a member", e.setting("member", "/UYELER/ARGE/WEBLAB", off), fiber.StatusForbidden, "")
	wantStatus(t, "a Sub-team's Leader on the Team above", e.setting("frontLeader", "/UYELER/ARGE/WEBLAB", off), fiber.StatusForbidden, "")
	wantStatus(t, "another Team's Leader", e.setting("mobLeader", "/UYELER/ARGE/WEBLAB", off), fiber.StatusForbidden, "")
	for _, row := range e.audit.Rows()[before:] {
		if row.Outcome != teamaudit.OutcomeDenied {
			t.Errorf("a refusal wrote %+v", row)
		}
	}
	wantStatus(t, "a Leader group", e.setting("yk", "/UYELER/ARGE/WEBLAB/LIDERLER", off), fiber.StatusBadRequest, "")
	wantStatus(t, "an area", e.setting("yk", "/UYELER/ARGE", off), fiber.StatusBadRequest, "")
	wantStatus(t, "no value", e.setting("webLeader", "/UYELER/ARGE/WEBLAB", `{}`), fiber.StatusBadRequest, "")
}

func TestTeamSettingsDelegationOff(t *testing.T) {
	t.Parallel()
	e := newDelegationEnv(t, delegationSetup{})
	wantStatus(t, "a Leader", e.setting("webLeader", "/UYELER/ARGE/WEBLAB", `{"membersManageCompetitors":false}`), fiber.StatusForbidden, "")
	if v := e.attribute("/UYELER/ARGE/WEBLAB", "members_manage_competitors"); v != "" {
		t.Errorf("attribute %q written with delegation off", v)
	}
	wantStatus(t, "YK", e.setting("yk", "/UYELER/ARGE/WEBLAB", `{"membersManageCompetitors":false}`), fiber.StatusOK, "")
}

// A protected Group (one that maps an admin role) is Privileged's: its
// Leader gets 403 protected_group there too.
func TestTeamSettingsProtectedGroup(t *testing.T) {
	t.Parallel()
	e := newDelegationEnv(t, delegationSetup{on: true})
	wantStatus(t, "a Leader above a protected Team", e.setting("webLeader", "/UYELER/ARGE/WEBLAB/SECURE", `{"membersManageCompetitors":false}`),
		fiber.StatusForbidden, "protected_group")
	if v := e.attribute("/UYELER/ARGE/WEBLAB/SECURE", "members_manage_competitors"); v != "" {
		t.Errorf("attribute %q written on a protected Group", v)
	}
}

// End to end, as main wires it: the Leader turns it off, a member's
// competitor add is 403 at once (the write forgot the kept tree), the
// Leader's goes through; on again, the member adds.
func TestTeamSettingsDecideCompetitorWrites(t *testing.T) {
	t.Parallel()
	e := newDelegationEnv(t, delegationSetup{on: true, teamTree: true})
	yk := e.token("yk")
	created := e.send(yk, fiber.MethodPost, "/v1/events", `{"name":"Hack","location":"YTÜ","ownerTeam":"WEBLAB"}`)
	wantStatus(t, "event", created, fiber.StatusCreated, "")
	var ev struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(created.body, &ev); err != nil {
		t.Fatal(err)
	}
	wantStatus(t, "publish", e.send(yk, fiber.MethodPost, "/v1/events/"+ev.ID+"/publish", ""), fiber.StatusOK, "")
	add := func(as, who string) rawResponse {
		return e.send(e.token(as), fiber.MethodPost, "/v1/competitors",
			`{"eventId":"`+ev.ID+`","userId":"`+e.people[who].String()+`"}`)
	}
	// Read the tree once, so a kept answer is there to forget.
	wantStatus(t, "a member, the setting unset", add("member", "outsider"), fiber.StatusCreated, "")

	forgets := e.tree.forgotten()
	wantStatus(t, "off", e.setting("webLeader", "/UYELER/ARGE/WEBLAB", `{"membersManageCompetitors":false}`), fiber.StatusOK, "")
	if e.tree.forgotten() == forgets {
		t.Error("the settings write did not forget the kept tree")
	}
	wantStatus(t, "a member, the setting off", add("member", "newcomer"), fiber.StatusForbidden, "")
	wantStatus(t, "the Leader, the setting off", add("webLeader", "newcomer"), fiber.StatusCreated, "")

	wantStatus(t, "on", e.setting("webLeader", "/UYELER/ARGE/WEBLAB", `{"membersManageCompetitors":true}`), fiber.StatusOK, "")
	wantStatus(t, "a member, the setting on again", add("member", "exLeader"), fiber.StatusCreated, "")
}
