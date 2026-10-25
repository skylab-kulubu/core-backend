package httpx_test

import (
	"context"
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
