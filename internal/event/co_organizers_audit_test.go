package event_test

import (
	"context"
	"errors"
	"maps"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/identity"
	"github.com/skylab-kulubu/core-backend/internal/teamaudit"
)

func withTeam(teams fakeTeams, code, path string) fakeTeams {
	out := maps.Clone(teams)
	out[code] = path
	return out
}

type auditWorld struct {
	svc    event.Service
	audit  *teamaudit.Memory
	ev     event.Event
	noTeam event.Event
	ctx    func(p authz.Principal) context.Context
}

func newAuditWorld(t *testing.T) auditWorld {
	t.Helper()
	dir := identity.NewMemory()
	dir.PutGroup(identity.Group{ID: "8a0c7f2e-0000-4000-8000-000000000001", Name: "ARTLAB", Path: "/UYELER/SANAT/ARTLAB"})
	dir.PutGroup(identity.Group{ID: "8a0c7f2e-0000-4000-8000-000000000002", Name: "SANAT", Path: "/UYELER/SANAT"})
	audit := teamaudit.NewMemory()
	store := event.NewMemoryStore()
	svc := event.NewServiceWithOptions(store, authz.NewAuthorizer(authz.DefaultPolicy()), event.ServiceOptions{
		Teams: withTeam(organizerTeams, "SANAT", "/UYELER/SANAT"), CoOrganizersWrite: true, Now: listClock, Audit: audit, Groups: dir,
	})
	ev, err := svc.Create(context.Background(), ykMember, event.Event{Name: "Hack", Location: "YTÜ", OwnerTeam: "ARTLAB", Status: event.StatusPublished})
	if err != nil {
		t.Fatal(err)
	}
	noTeam, err := svc.Create(context.Background(), ykMember, event.Event{Name: "Kulüp", Location: "YTÜ", Status: event.StatusPublished})
	if err != nil {
		t.Fatal(err)
	}
	return auditWorld{svc: svc, audit: audit, ev: ev, noTeam: noTeam, ctx: func(p authz.Principal) context.Context {
		id, err := uuid.Parse(p.ID)
		if err != nil {
			id = uuid.New()
		}
		return teamaudit.WithActor(context.Background(), teamaudit.Actor{ID: id, Basis: teamaudit.BasisPrivileged, Client: "panel"})
	}}
}

// Every change of the list is a row of the Team audit record against the
// Event's Owner team (docs/co-organizers.md): a team and a person added, a
// team's choices changed, a person removed, each its own row with the
// Event, the team's code and choices or the person's id; the same list again
// writes none. YK is privileged; the Owner team's Leader records the Team
// whose Leader group grants it.
func TestCoOrganizerChangesAreAudited(t *testing.T) {
	t.Parallel()
	w := newAuditWorld(t)
	person := uuid.New()
	yk := authz.Principal{ID: uuid.NewString(), Groups: []string{"/UYELER/YK"}}
	if _, err := w.svc.SetOrganizers(w.ctx(yk), yk, w.ev.ID, event.OrganizersInput{
		Teams: []event.OrganizerTeamInput{{Code: "WEBLAB"}}, People: []uuid.UUID{person},
	}); err != nil {
		t.Fatal(err)
	}
	rows := w.audit.Rows()
	if len(rows) != 2 {
		t.Fatalf("rows %+v", rows)
	}
	team, who := rows[0], rows[1]
	if team.Action != teamaudit.ActionCoOrganizerAdd || team.Outcome != teamaudit.OutcomeOK || team.Actor.Basis != teamaudit.BasisPrivileged ||
		team.GroupPath != "/UYELER/SANAT/ARTLAB" || team.After == nil || team.After.Code != "WEBLAB" || team.After.Management != "leaders" ||
		team.After.Visibility != "co_organizer" || team.After.EventID != w.ev.ID.String() || team.Before != nil {
		t.Fatalf("team row %+v %+v", team, team.After)
	}
	if who.Action != teamaudit.ActionCoOrganizerAdd || who.After == nil || who.After.PersonID != person.String() || who.After.Code != "" {
		t.Fatalf("person row %+v %+v", who, who.After)
	}

	leader := authz.Principal{ID: uuid.NewString(), Groups: []string{"/UYELER/SANAT/ARTLAB/LIDERLER"}}
	if _, err := w.svc.SetOrganizers(w.ctx(leader), leader, w.ev.ID, event.OrganizersInput{
		Teams: []event.OrganizerTeamInput{{Code: "WEBLAB", Management: ptr(event.ManagementMembers)}},
	}); err != nil {
		t.Fatal(err)
	}
	rows = w.audit.Rows()[2:]
	if len(rows) != 2 || rows[0].Action != teamaudit.ActionCoOrganizerChange || rows[0].Before.Management != "leaders" || rows[0].After.Management != "members" ||
		rows[1].Action != teamaudit.ActionCoOrganizerRemove || rows[1].Before.PersonID != person.String() {
		t.Fatalf("change and removal %+v", rows)
	}
	for _, row := range rows {
		if row.Actor.Basis != teamaudit.BasisLeader || row.Actor.ViaGroupID != "8a0c7f2e-0000-4000-8000-000000000001" {
			t.Fatalf("the Owner team's Leader: %+v", row.Actor)
		}
	}
	if _, err := w.svc.SetOrganizers(w.ctx(leader), leader, w.ev.ID, event.OrganizersInput{
		Teams: []event.OrganizerTeamInput{{Code: "WEBLAB", Management: ptr(event.ManagementMembers)}},
	}); err != nil || len(w.audit.Rows()) != 4 {
		t.Fatalf("the same list again: %d rows %v", len(w.audit.Rows()), err)
	}

	// An Event without an Owner team: Privileged alone, rows without a Group.
	if _, err := w.svc.SetOrganizers(w.ctx(yk), yk, w.noTeam.ID, event.OrganizersInput{Teams: []event.OrganizerTeamInput{{Code: "WEBLAB"}}}); err != nil {
		t.Fatal(err)
	}
	last := w.audit.Rows()[4]
	if last.GroupID != "" || last.GroupPath != "" || last.Outcome != teamaudit.OutcomeOK || last.After.EventID != w.noTeam.ID.String() {
		t.Fatalf("no Owner team: %+v", last)
	}
}

// A write authorization refuses is recorded denied, a row for each change
// it asked for; a write whose row cannot be opened is refused
// (teamaudit.ErrUnavailable) and changes nothing.
func TestCoOrganizerChangesRefusedAndUnrecorded(t *testing.T) {
	t.Parallel()
	w := newAuditWorld(t)
	co := authz.Principal{ID: uuid.NewString(), Groups: []string{"/UYELER/ARGE/WEBLAB/LIDERLER"}}
	if _, err := w.svc.SetOrganizers(w.ctx(co), co, w.ev.ID, event.OrganizersInput{Teams: []event.OrganizerTeamInput{{Code: "GECEKODU"}}}); !errors.Is(err, event.ErrForbidden) {
		t.Fatalf("a stranger changes the list: %v", err)
	}
	rows := w.audit.Rows()
	if len(rows) != 1 || rows[0].Outcome != teamaudit.OutcomeDenied || rows[0].Actor.Basis != teamaudit.BasisNone ||
		rows[0].Action != teamaudit.ActionCoOrganizerAdd || rows[0].After.Code != "GECEKODU" || rows[0].ErrorCode != "forbidden" {
		t.Fatalf("denied %+v", rows)
	}

	yk := authz.Principal{ID: uuid.NewString(), Groups: []string{"/UYELER/YK"}}
	w.audit.SetUnavailable(true)
	if _, err := w.svc.SetOrganizers(w.ctx(yk), yk, w.ev.ID, event.OrganizersInput{Teams: []event.OrganizerTeamInput{{Code: "WEBLAB"}}}); !errors.Is(err, teamaudit.ErrUnavailable) {
		t.Fatalf("written without its row: %v", err)
	}
	w.audit.SetUnavailable(false)
	got, err := w.svc.Organizers(context.Background(), yk, w.ev.ID)
	if err != nil || len(got.Teams) != 0 {
		t.Fatalf("the list changed: %+v %v", got.Teams, err)
	}
	// Without an actor in the request nothing is recorded, so nothing is written.
	if _, err := w.svc.SetOrganizers(context.Background(), yk, w.ev.ID, event.OrganizersInput{Teams: []event.OrganizerTeamInput{{Code: "WEBLAB"}}}); !errors.Is(err, teamaudit.ErrUnavailable) {
		t.Fatalf("no actor: %v", err)
	}
}
