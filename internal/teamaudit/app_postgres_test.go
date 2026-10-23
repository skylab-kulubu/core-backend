package teamaudit_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/teamaudit"
)

// App rows go in for a Group and for a person (no path), come back newest
// first by client, and the latest per target and role is the newest that
// ended ok. The table refuses an app row with two targets or none.
func TestPostgresAppRows(t *testing.T) {
	pool := migrated(t)
	ctx := context.Background()
	w := teamaudit.NewPostgres(pool)
	actor := teamaudit.Actor{ID: uuid.New(), Basis: teamaudit.BasisPrivileged, Client: "admin"}
	groupID, person := uuid.NewString(), uuid.New()
	group := teamaudit.Record{
		Actor: actor, Action: teamaudit.ActionAppAssign, GroupID: groupID, GroupPath: "/UYELER/ARGE/WEBLAB",
		After: &teamaudit.State{ID: groupID, Path: "/UYELER/ARGE/WEBLAB", Client: "forms", Role: "skyforms:access", Note: "ekip talebi"},
	}
	user := teamaudit.Record{
		Actor: actor, Action: teamaudit.ActionAppAssign, TargetUserID: person,
		After: &teamaudit.State{Client: "forms", Role: "skyforms:access"},
	}
	finish := func(r teamaudit.Record, outcome teamaudit.Outcome, code string) uuid.UUID {
		t.Helper()
		id, err := w.Begin(ctx, r)
		if err != nil {
			t.Fatal(err)
		}
		if err := w.Finish(ctx, id, outcome, code, nil); err != nil {
			t.Fatal(err)
		}
		return id
	}
	assigned := finish(group, teamaudit.OutcomeOK, "")
	userAssigned := finish(user, teamaudit.OutcomeOK, "")
	removal := user
	removal.Action = teamaudit.ActionAppUnassign
	userRemoved := finish(removal, teamaudit.OutcomeOK, "")
	finish(group, teamaudit.OutcomeFailed, "keycloak_400")
	dk := user
	dk.Actor.Basis = teamaudit.BasisNone
	if err := w.Deny(ctx, dk, "forbidden"); err != nil {
		t.Fatal(err)
	}
	mail := group
	mail.After = &teamaudit.State{ID: groupID, Path: group.GroupPath, Client: "skymail", Role: "skymail:access"}
	finish(mail, teamaudit.OutcomeOK, "")

	rows, err := w.AppRows(ctx, "forms", 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 5 || rows[0].Outcome != teamaudit.OutcomeDenied || rows[4].ID != assigned {
		t.Fatalf("rows %+v", rows)
	}
	if got := rows[4]; got.GroupID != groupID || got.GroupPath != "/UYELER/ARGE/WEBLAB" || got.After == nil ||
		got.After.Note != "ekip talebi" || got.Actor.ID != actor.ID || got.TargetUserID != uuid.Nil {
		t.Errorf("group row %+v", got)
	}
	if got := rows[0]; got.TargetUserID != person || got.GroupPath != "" || got.GroupID != "" || got.Actor.Basis != teamaudit.BasisNone {
		t.Errorf("denied row %+v", got)
	}
	if limited, err := w.AppRows(ctx, "forms", 2); err != nil || len(limited) != 2 {
		t.Errorf("limit %d %v", len(limited), err)
	}
	latest, err := w.LatestAppRows(ctx, "forms")
	if err != nil {
		t.Fatal(err)
	}
	ids := map[uuid.UUID]teamaudit.Action{}
	for _, r := range latest {
		ids[r.ID] = r.Action
	}
	if len(latest) != 2 || ids[assigned] != teamaudit.ActionAppAssign || ids[userRemoved] != teamaudit.ActionAppUnassign {
		t.Errorf("latest %+v (assigned %s, user %s/%s)", latest, assigned, userAssigned, userRemoved)
	}

	for name, q := range map[string]string{
		"two targets": `INSERT INTO team_audit_records (id, occurred_at, actor_id, actor_basis, action, group_id, group_path, target_user_id, outcome)
			VALUES (gen_random_uuid(), now(), gen_random_uuid(), 'privileged', 'app.assign', gen_random_uuid(), '/UYELER/A/B', gen_random_uuid(), 'ok')`,
		"no target": `INSERT INTO team_audit_records (id, occurred_at, actor_id, actor_basis, action, group_path, outcome)
			VALUES (gen_random_uuid(), now(), gen_random_uuid(), 'privileged', 'app.assign', '', 'ok')`,
		"a person on a member action without a path": `INSERT INTO team_audit_records (id, occurred_at, actor_id, actor_basis, action, group_path, target_user_id, outcome)
			VALUES (gen_random_uuid(), now(), gen_random_uuid(), 'privileged', 'member.add', '', gen_random_uuid(), 'ok')`,
		"a person on group.roles": `INSERT INTO team_audit_records (id, occurred_at, actor_id, actor_basis, action, group_id, group_path, target_user_id, outcome)
			VALUES (gen_random_uuid(), now(), gen_random_uuid(), 'privileged', 'group.roles', gen_random_uuid(), '/A', gen_random_uuid(), 'ok')`,
	} {
		if _, err := pool.Exec(ctx, q); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
