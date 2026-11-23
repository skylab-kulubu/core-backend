package httpx_test

import (
	"context"
	"encoding/json"
	"slices"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/identity"
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

// setAttribute writes a Group attribute straight into the directory, as an
// older core or the Keycloak console left it.
func (e *delegationEnv) setAttribute(path, name, value string) {
	e.t.Helper()
	g, err := e.dir.GetGroup(context.Background(), e.ids[path])
	if err != nil {
		e.t.Fatal(err)
	}
	attrs := map[string]string{}
	for k, v := range g.Attributes {
		attrs[k] = v
	}
	attrs[name] = value
	g.Attributes = attrs
	if _, err := e.dir.UpdateGroup(context.Background(), g); err != nil {
		e.t.Fatal(err)
	}
}

// The retired members_manage_competitors (Yusuf, 2026-11-23) is no setting
// any more: the route takes it from nobody, writes nothing and answers
// without it.
func TestTeamSettingsRetiredMembersManageCompetitors(t *testing.T) {
	t.Parallel()
	e := newDelegationEnv(t, delegationSetup{on: true})
	web := "/UYELER/ARGE/WEBLAB"
	before := len(e.audit.Rows())
	for _, as := range []string{"webLeader", "yk"} {
		for _, body := range []string{`{"membersManageCompetitors":false}`, `{"membersManageCompetitors":true}`,
			`{"membersManageCompetitors":true,"competitionManagers":"members"}`} {
			wantStatus(t, as+" "+body, e.setting(as, web, body), fiber.StatusBadRequest, "")
		}
	}
	if v := e.attribute(web, "members_manage_competitors"); v != "" {
		t.Errorf("attribute %q written", v)
	}
	if v := e.attribute(web, "team_competition_manage"); v != "" {
		t.Errorf("a refused body wrote team_competition_manage %q", v)
	}
	if rows := e.audit.Rows()[before:]; len(rows) != 0 {
		t.Errorf("a 400 wrote %+v", rows)
	}
	got := e.setting("webLeader", web, `{"competitionManagers":"leaders"}`)
	wantStatus(t, "the other setting", got, fiber.StatusOK, "")
	if want := `{"competitionManagers":"leaders"}`; string(got.body) != want {
		t.Errorf("answer %s, want %s", got.body, want)
	}
}

// End to end, as main wires it (Yusuf, 2026-11-23: "Ahmet Kaya olsam Yusuf
// İnan diye birini ekleyebilirim"): a plain member of the Owner team enters
// and withdraws only themselves, whatever members_manage_competitors an
// older core left on the Team; the Leader, a coordinator and YK add anyone;
// "Yarışmaları kim yönetir: Tüm üyeler" lets the members add others too.
func TestCompetitorWritesAreTheEventManagers(t *testing.T) {
	t.Parallel()
	e := newDelegationEnv(t, delegationSetup{on: true, teamTree: true})
	web := "/UYELER/ARGE/WEBLAB"
	e.setAttribute(web, "members_manage_competitors", "true")
	// GECEKODU, whose members edit its Events (the Event policy) but manage
	// none of its Competitions.
	gecekodu := "/UYELER/ORGANIZASYON/GECEKODU"
	e.ids[gecekodu] = uuid.NewString()
	e.dir.PutGroup(identity.Group{ID: e.ids[gecekodu], Name: "GECEKODU", Path: gecekodu})
	e.person("gecekoduMember", gecekodu)
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
			`{"eventId":"`+ev.ID+`","userId":"`+e.people[who].String()+`","score":90,"isWinner":true}`)
	}
	id := func(r rawResponse) string {
		t.Helper()
		var c struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(r.body, &c); err != nil {
			t.Fatal(err)
		}
		return c.ID
	}

	wantStatus(t, "a member adds someone else", add("member", "outsider"), fiber.StatusForbidden, "")
	wantStatus(t, "a Sub-team member adds someone else", add("frontMember", "outsider"), fiber.StatusForbidden, "")
	wantStatus(t, "an outsider adds someone else", add("outsider", "newcomer"), fiber.StatusForbidden, "")
	own := add("member", "member")
	wantStatus(t, "a member enters themselves", own, fiber.StatusCreated, "")
	var self struct {
		Score    *float64 `json:"score"`
		IsWinner bool     `json:"isWinner"`
	}
	if err := json.Unmarshal(own.body, &self); err != nil || self.Score != nil || self.IsWinner {
		t.Errorf("a member gave themselves %s", own.body)
	}
	wantStatus(t, "an outsider enters themselves", add("outsider", "outsider"), fiber.StatusCreated, "")

	leaders := add("webLeader", "newcomer")
	wantStatus(t, "the Leader adds someone", leaders, fiber.StatusCreated, "")
	wantStatus(t, "a coordinator adds someone", add("webCoord", "frontMember"), fiber.StatusCreated, "")
	wantStatus(t, "YK adds someone", add("yk", "exLeader"), fiber.StatusCreated, "")
	wantStatus(t, "another Team's Leader", add("mobLeader", "webCoord"), fiber.StatusForbidden, "")

	other := id(leaders)
	member := e.token("member")
	wantStatus(t, "a member edits someone else", e.send(member, fiber.MethodPut, "/v1/competitors/"+other, `{"score":1}`), fiber.StatusForbidden, "")
	wantStatus(t, "a member removes someone else", e.send(member, fiber.MethodDelete, "/v1/competitors/"+other, ""), fiber.StatusForbidden, "")
	wantStatus(t, "a member edits their own score", e.send(member, fiber.MethodPut, "/v1/competitors/"+id(own), `{"score":100}`), fiber.StatusForbidden, "")
	wantStatus(t, "a member withdraws themselves", e.send(member, fiber.MethodDelete, "/v1/competitors/"+id(own), ""), fiber.StatusNoContent, "")
	wantStatus(t, "the Leader edits", e.send(e.token("webLeader"), fiber.MethodPut, "/v1/competitors/"+other, `{"score":70}`), fiber.StatusOK, "")

	// Withdrawn by the Leader, the member does not bring themselves back
	// (review T1); withdrawn by themselves, they do.
	wantStatus(t, "a member brings back their own withdrawal", e.send(member, fiber.MethodPost, "/v1/competitors/"+id(own)+"/reinstate", ""), fiber.StatusOK, "")
	leader := e.token("webLeader")
	wantStatus(t, "the Leader withdraws the member", e.send(leader, fiber.MethodDelete, "/v1/competitors/"+id(own), ""), fiber.StatusNoContent, "")
	wantStatus(t, "a member brings back the Leader's withdrawal", e.send(member, fiber.MethodPost, "/v1/competitors/"+id(own)+"/reinstate", ""), fiber.StatusForbidden, "")
	wantStatus(t, "the Leader brings it back", e.send(leader, fiber.MethodPost, "/v1/competitors/"+id(own)+"/reinstate", ""), fiber.StatusOK, "")

	// Nobody scores themselves, the Leader neither (review T2).
	leaderOwn := add("webLeader", "webLeader")
	wantStatus(t, "the Leader enters themselves", leaderOwn, fiber.StatusCreated, "")
	wantStatus(t, "the Leader scores themselves", e.send(leader, fiber.MethodPut, "/v1/competitors/"+id(leaderOwn), `{"score":100,"isWinner":true}`), fiber.StatusForbidden, "own_competitor")
	wantStatus(t, "YK scores the Leader", e.send(yk, fiber.MethodPut, "/v1/competitors/"+id(leaderOwn), `{"score":100}`), fiber.StatusOK, "")

	// The capabilities say so: the panel hides "Yarışmacı ekle" from a member.
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
		if team.Team == "WEBLAB" && (team.Can["competitor.create"] || team.Can["competitor.update"] || team.Can["competitor.delete"] || !team.Can["competitor.read"]) {
			t.Errorf("a member's WEBLAB competitor capabilities %v", team.Can)
		}
	}

	// Editing the Event is not managing its Competitions (Yusuf, 2026-11-23):
	// a GECEKODU member adds only themselves to GECEKODU's Event.
	gk := e.send(yk, fiber.MethodPost, "/v1/events", `{"name":"Gece","location":"YTÜ","ownerTeam":"GECEKODU"}`)
	wantStatus(t, "GECEKODU event", gk, fiber.StatusCreated, "")
	var gkEv struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(gk.body, &gkEv); err != nil {
		t.Fatal(err)
	}
	wantStatus(t, "publish GECEKODU", e.send(yk, fiber.MethodPost, "/v1/events/"+gkEv.ID+"/publish", ""), fiber.StatusOK, "")
	addTo := func(as, who string) rawResponse {
		return e.send(e.token(as), fiber.MethodPost, "/v1/competitors",
			`{"eventId":"`+gkEv.ID+`","userId":"`+e.people[who].String()+`"}`)
	}
	wantStatus(t, "a GECEKODU member adds someone else", addTo("gecekoduMember", "outsider"), fiber.StatusForbidden, "")
	wantStatus(t, "a GECEKODU member enters themselves", addTo("gecekoduMember", "gecekoduMember"), fiber.StatusCreated, "")
	wantStatus(t, "YK adds to GECEKODU", addTo("yk", "outsider"), fiber.StatusCreated, "")

	wantStatus(t, "all members manage", e.setting("webLeader", web, `{"competitionManagers":"members"}`), fiber.StatusOK, "")
	wantStatus(t, "a member, all members manage", add("member", "mobLeader"), fiber.StatusCreated, "")
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

	before := len(e.audit.Rows())
	got := e.setting("webLeader", web, `{"competitionManagers":"members"}`)
	wantStatus(t, "the Team's Leader opens it", got, fiber.StatusOK, "")
	if want := `{"competitionManagers":"members"}`; string(got.body) != want {
		t.Errorf("answer %s, want %s", got.body, want)
	}
	if v := e.attribute(web, "team_competition_manage"); v != "true" {
		t.Errorf("attribute %q, want true", v)
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
