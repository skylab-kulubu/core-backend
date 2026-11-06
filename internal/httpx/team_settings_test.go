package httpx_test

import (
	"context"
	"encoding/json"
	"slices"
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
	if want := `{"membersManageCompetitors":false,"competitionManagers":"leaders"}`; string(got.body) != want {
		t.Errorf("answer %s, want %s", got.body, want)
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

// "Yarışmaları kim yönetir" (Yusuf, 2026-10-12 and 2026-10-31): the Team's
// manager, its own Leader included, opens its Competitions to all members
// though the setting widens (ADR-0065's exception for it); a member is
// refused with a denied row; the other settings and attributes stay; an
// unknown field or value is 400; no audit row, no change.
func TestTeamSettingsCompetitionManagers(t *testing.T) {
	t.Parallel()
	e := newDelegationEnv(t, delegationSetup{on: true})
	web := "/UYELER/ARGE/WEBLAB"
	wantStatus(t, "competitors off", e.setting("webLeader", web, `{"membersManageCompetitors":false}`), fiber.StatusOK, "")

	before := len(e.audit.Rows())
	got := e.setting("webLeader", web, `{"competitionManagers":"members"}`)
	wantStatus(t, "the Team's Leader opens it", got, fiber.StatusOK, "")
	if want := `{"membersManageCompetitors":false,"competitionManagers":"members"}`; string(got.body) != want {
		t.Errorf("answer %s, want %s", got.body, want)
	}
	if v := e.attribute(web, "team_competition_manage"); v != "true" {
		t.Errorf("attribute %q, want true", v)
	}
	if v := e.attribute(web, "members_manage_competitors"); v != "false" {
		t.Errorf("the competitor setting became %q", v)
	}
	rows := e.audit.Rows()[before:]
	if len(rows) != 1 || rows[0].Action != teamaudit.ActionGroupAttributes || rows[0].Outcome != teamaudit.OutcomeOK ||
		rows[0].Before == nil || rows[0].Before.Attributes["team_competition_manage"] != "" ||
		rows[0].After == nil || rows[0].After.Attributes["team_competition_manage"] != "true" ||
		rows[0].Actor.Basis != teamaudit.BasisLeader {
		t.Errorf("rows %+v, want one group.attributes row by a Leader with the old and new value", rows)
	}

	wantStatus(t, "a Leader above on a Sub-team", e.setting("webLeader", web+"/FRONT", `{"competitionManagers":"members"}`), fiber.StatusOK, "")
	wantStatus(t, "YK closes it", e.setting("yk", web+"/FRONT", `{"competitionManagers":"leaders"}`), fiber.StatusOK, "")
	if v := e.attribute(web+"/FRONT", "team_competition_manage"); v != "false" {
		t.Errorf("FRONT attribute %q, want false", v)
	}

	before = len(e.audit.Rows())
	for name, as := range map[string]string{"a member": "member", "a Sub-team's Leader on the Team above": "frontLeader", "another Team's Leader": "mobLeader"} {
		wantStatus(t, name, e.setting(as, web, `{"competitionManagers":"leaders"}`), fiber.StatusForbidden, "")
	}
	for _, row := range e.audit.Rows()[before:] {
		if row.Outcome != teamaudit.OutcomeDenied {
			t.Errorf("a refusal wrote %+v", row)
		}
	}
	if v := e.attribute(web, "team_competition_manage"); v != "true" {
		t.Errorf("a refused write changed it to %q", v)
	}
	wantStatus(t, "a protected Group", e.setting("webLeader", web+"/SECURE", `{"competitionManagers":"members"}`), fiber.StatusForbidden, "protected_group")

	for name, body := range map[string]string{
		"an unknown value":         `{"competitionManagers":"everyone"}`,
		"a Privileged attribute":   `{"competitionManagers":"members","teamDoorScan":true}`,
		"the attribute's own name": `{"team_competition_manage":"true"}`,
		"a boolean":                `{"competitionManagers":true}`,
		"null alone":               `{"competitionManagers":null}`,
		"two documents":            `{"competitionManagers":"members"}{}`,
	} {
		wantStatus(t, name, e.setting("yk", web, body), fiber.StatusBadRequest, "")
	}

	e.audit.SetUnavailable(true)
	ops := len(e.dir.Ops)
	wantStatus(t, "audit down", e.setting("webLeader", web, `{"competitionManagers":"leaders"}`), fiber.StatusServiceUnavailable, "audit_unavailable")
	if slices.Contains(e.dir.Ops[ops:], "UpdateGroup") || e.attribute(web, "team_competition_manage") != "true" {
		t.Error("changed without an audit row")
	}
}

func TestTeamSettingsCompetitionManagersDelegationOff(t *testing.T) {
	t.Parallel()
	e := newDelegationEnv(t, delegationSetup{})
	wantStatus(t, "a Leader", e.setting("webLeader", "/UYELER/ARGE/WEBLAB", `{"competitionManagers":"members"}`), fiber.StatusForbidden, "")
	if v := e.attribute("/UYELER/ARGE/WEBLAB", "team_competition_manage"); v != "" {
		t.Errorf("attribute %q written with delegation off", v)
	}
	wantStatus(t, "YK", e.setting("yk", "/UYELER/ARGE/WEBLAB", `{"competitionManagers":"members"}`), fiber.StatusOK, "")
}

// End to end, as main wires it: with the setting at its default a member's
// Competition is 403; the Leader opens it and the member creates, edits
// and sees the tab in the capabilities, but does not archive; closed again,
// the member is refused at once.
func TestTeamSettingsDecideCompetitionWrites(t *testing.T) {
	t.Parallel()
	e := newDelegationEnv(t, delegationSetup{on: true, teamTree: true})
	yk := e.token("yk")
	created := e.send(yk, fiber.MethodPost, "/v1/events", `{"name":"AGC","location":"YTÜ","ownerTeam":"WEBLAB"}`)
	wantStatus(t, "event", created, fiber.StatusCreated, "")
	var ev struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(created.body, &ev); err != nil {
		t.Fatal(err)
	}
	wantStatus(t, "publish", e.send(yk, fiber.MethodPost, "/v1/events/"+ev.ID+"/publish", ""), fiber.StatusOK, "")
	member := e.token("member")
	create := func() rawResponse {
		return e.send(member, fiber.MethodPost, "/v1/events/"+ev.ID+"/competitions", `{"name":"Hafta 1"}`)
	}
	capabilities := func() map[string]bool {
		t.Helper()
		got := e.send(member, fiber.MethodGet, "/v1/users/me/capabilities", "")
		wantStatus(t, "capabilities", got, fiber.StatusOK, "")
		var caps struct {
			Teams []struct {
				Team string          `json:"team"`
				Can  map[string]bool `json:"can"`
			} `json:"teams"`
		}
		if err := json.Unmarshal(got.body, &caps); err != nil {
			t.Fatal(err)
		}
		for _, team := range caps.Teams {
			if team.Team == "WEBLAB" {
				return team.Can
			}
		}
		t.Fatalf("no WEBLAB scope: %s", got.body)
		return nil
	}

	wantStatus(t, "a member, the default", create(), fiber.StatusForbidden, "")
	if can := capabilities(); can["competition.manage"] {
		t.Errorf("default: %v", can)
	}

	wantStatus(t, "open", e.setting("webLeader", "/UYELER/ARGE/WEBLAB", `{"competitionManagers":"members"}`), fiber.StatusOK, "")
	got := create()
	wantStatus(t, "a member, the setting on", got, fiber.StatusCreated, "")
	var c struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(got.body, &c); err != nil {
		t.Fatal(err)
	}
	if can := capabilities(); !can["competition.manage"] || can["competition.archive"] {
		t.Errorf("setting on: %v", can)
	}
	wantStatus(t, "a member edits", e.send(member, fiber.MethodPatch, "/v1/competitions/"+c.ID, `{"name":"Hafta 1 (AGC)"}`), fiber.StatusOK, "")
	wantStatus(t, "a member's candidates", e.send(member, fiber.MethodGet, "/v1/competitions/"+c.ID+"/entrant-candidates?checkedIn=false", ""), fiber.StatusOK, "")
	wantStatus(t, "a bad checkedIn", e.send(member, fiber.MethodGet, "/v1/competitions/"+c.ID+"/entrant-candidates?checkedIn=yes", ""), fiber.StatusBadRequest, "")
	wantStatus(t, "a member archives", e.send(member, fiber.MethodPost, "/v1/competitions/"+c.ID+"/archive", ""), fiber.StatusForbidden, "")
	wantStatus(t, "an outsider", e.send(e.token("outsider"), fiber.MethodPatch, "/v1/competitions/"+c.ID, `{"name":"x"}`), fiber.StatusForbidden, "")

	wantStatus(t, "close", e.setting("webLeader", "/UYELER/ARGE/WEBLAB", `{"competitionManagers":"leaders"}`), fiber.StatusOK, "")
	wantStatus(t, "a member, closed again", e.send(member, fiber.MethodPatch, "/v1/competitions/"+c.ID, `{"name":"y"}`), fiber.StatusForbidden, "")
	wantStatus(t, "the Leader archives", e.send(e.token("webLeader"), fiber.MethodPost, "/v1/competitions/"+c.ID+"/archive", ""), fiber.StatusNoContent, "")
}
