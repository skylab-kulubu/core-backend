package identity_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/identity"
	"github.com/skylab-kulubu/core-backend/internal/teamaudit"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// A member write made with a way in its context (teamaudit.WithVia) is
// recorded with it: a Privileged add, a Leader's delegated add and a
// Leader's refused add alike, with the actor and basis of the person who
// made it. A write without one is recorded as before, without a way. The
// reading route answers each row's way.
func TestMemberWritesCarryTheirWay(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	m := identity.NewMemory()
	ids := map[string]string{}
	for _, path := range []string{"/UYELER", "/UYELER/YK", "/UYELER/ARGE", "/UYELER/ARGE/WEBLAB", "/UYELER/ARGE/WEBLAB/LIDERLER", "/UYELER/ARGE/MOBILAB"} {
		ids[path] = uuid.NewString()
		m.PutGroup(identity.Group{ID: ids[path], Name: lastSegment(path), Path: path})
	}
	// YK maps core's groups:manage: the protection canary delegated writes
	// wait for.
	if err := m.SetGroupClientRoles(ctx, ids["/UYELER/YK"], []identity.ClientRole{{ClientID: "core", Role: authz.RoleGroupsManage}}); err != nil {
		t.Fatal(err)
	}
	yk, leader, ada, bob, cem, dan := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	users := user.NewMemoryStore()
	for _, id := range []uuid.UUID{yk, leader, ada, bob, cem, dan} {
		m.PutUser(identity.Person{ID: id, Email: id.String() + "@example.test"})
		if _, _, err := users.Upsert(ctx, user.User{ID: id, Email: id.String() + "@example.test"}); err != nil {
			t.Fatal(err)
		}
	}
	for path, id := range map[string]uuid.UUID{"/UYELER/YK": yk, "/UYELER/ARGE/WEBLAB/LIDERLER": leader} {
		if err := m.AddMember(ctx, ids[path], id); err != nil {
			t.Fatal(err)
		}
	}
	audit := teamaudit.NewMemory()
	svc := identity.NewServiceWithOptions(identity.AuditedDirectory(m, audit, nil), users, authz.NewAuthorizer(onPolicy()), identity.Options{
		Delegation:   identity.NewTeamDelegation(onPolicy(), m, m, identity.TeamDelegationOptions{}),
		TeamAudit:    audit,
		AuditRecords: audit,
	})
	as := func(id uuid.UUID, via teamaudit.Via) context.Context {
		c := teamaudit.WithActor(ctx, teamaudit.Actor{ID: id, Basis: teamaudit.BasisPrivileged, Client: "core"})
		if via != "" {
			c = teamaudit.WithVia(c, via)
		}
		return c
	}
	ykP := authz.Principal{ID: yk.String(), Groups: []string{"/UYELER/YK"}}
	leaderP := authz.Principal{ID: leader.String()}

	if err := svc.AddMember(as(yk, ""), ykP, ids["/UYELER/ARGE/WEBLAB"], ada); err != nil {
		t.Fatal(err)
	}
	if err := svc.AddMember(as(yk, teamaudit.ViaInvitation), ykP, ids["/UYELER/ARGE/WEBLAB"], bob); err != nil {
		t.Fatal(err)
	}
	if err := svc.AddMember(as(leader, teamaudit.ViaAccessRequest), leaderP, ids["/UYELER/ARGE/WEBLAB"], cem); err != nil {
		t.Fatal(err)
	}
	if err := svc.AddMember(as(leader, teamaudit.ViaAccessRequest), leaderP, ids["/UYELER/ARGE/MOBILAB"], dan); err == nil {
		t.Fatal("a Leader added to another Team")
	}

	type row struct {
		basis   teamaudit.Basis
		outcome teamaudit.Outcome
		via     teamaudit.Via
	}
	want := map[uuid.UUID]row{
		ada: {teamaudit.BasisPrivileged, teamaudit.OutcomeOK, ""},
		bob: {teamaudit.BasisPrivileged, teamaudit.OutcomeOK, teamaudit.ViaInvitation},
		cem: {teamaudit.BasisLeader, teamaudit.OutcomeOK, teamaudit.ViaAccessRequest},
		dan: {teamaudit.BasisNone, teamaudit.OutcomeDenied, teamaudit.ViaAccessRequest},
	}
	rows := audit.Rows()
	if len(rows) != len(want) {
		t.Fatalf("rows %+v", rows)
	}
	for _, r := range rows {
		got := row{r.Actor.Basis, r.Outcome, r.Via}
		if r.Action != teamaudit.ActionMemberAdd || got != want[r.TargetUserID] {
			t.Fatalf("row of %s: %+v, want %+v", r.TargetUserID, got, want[r.TargetUserID])
		}
	}

	page, err := svc.TeamAuditRecords(ctx, authz.Principal{ID: uuid.NewString(), Roles: []string{authz.RoleAuditRead}}, identity.AuditQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != len(want) {
		t.Fatalf("items %+v", page.Items)
	}
	for _, item := range page.Items {
		if item.Target == nil || item.Via != string(want[item.Target.ID].via) {
			t.Fatalf("item %+v", item)
		}
	}
}
