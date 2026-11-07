package identity_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/identity"
)

// teamsRealm is the members' tree with ARGE/WEBLAB (Sub-teams FRONTEND,
// with FRONTEND/REACT below it, BACKEND and one named YK, each Team but YK
// with its Leader group), ARGE's MOBILAB, and a Privileged root /UYELER/YK
// with its own LIDERLER. Keys are paths.
func teamsRealm(t *testing.T) (*identity.Memory, map[string]string) {
	t.Helper()
	m := identity.NewMemory()
	ids := map[string]string{}
	for _, path := range []string{
		"/UYELER", "/UYELER/ARGE",
		"/UYELER/ARGE/WEBLAB", "/UYELER/ARGE/WEBLAB/LIDERLER", "/UYELER/ARGE/WEBLAB/KOORDINATORLER",
		"/UYELER/ARGE/WEBLAB/FRONTEND", "/UYELER/ARGE/WEBLAB/FRONTEND/LIDERLER",
		"/UYELER/ARGE/WEBLAB/FRONTEND/REACT", "/UYELER/ARGE/WEBLAB/FRONTEND/REACT/LIDERLER",
		"/UYELER/ARGE/WEBLAB/BACKEND", "/UYELER/ARGE/WEBLAB/BACKEND/KOORDINATORLER",
		"/UYELER/ARGE/WEBLAB/YK",
		"/UYELER/ARGE/MOBILAB", "/UYELER/ARGE/MOBILAB/LIDERLER",
		"/UYELER/YK", "/UYELER/YK/LIDERLER",
	} {
		id := uuid.NewString()
		ids[path] = id
		name := lastSegment(path)
		m.PutGroup(identity.Group{ID: id, Name: name, Path: path, Attributes: map[string]string{"display_name_tr": "Ad " + name}})
	}
	return m, ids
}

func teamsPerson(t *testing.T, m *identity.Memory, groups ...string) uuid.UUID {
	t.Helper()
	person := uuid.New()
	m.PutUser(identity.Person{ID: person, Email: person.String() + "@example.test"})
	for _, g := range groups {
		if err := m.AddMember(context.Background(), g, person); err != nil {
			t.Fatal(err)
		}
	}
	return person
}

// renderTeams is "path=role" for each Team, in the answer's order.
func renderTeams(teams []identity.PersonTeam) string {
	parts := make([]string, 0, len(teams))
	for _, team := range teams {
		parts = append(parts, strings.TrimPrefix(team.Path, "/UYELER/ARGE/")+"="+team.Role)
	}
	return strings.Join(parts, " ")
}

// A person's Teams are read with delegation off: a Leader (a coordinator
// too) leads their Team and every Sub-team below, Leader groups left out; a
// Sub-team's Leader leads only their own subtree and is a member of the
// Teams above; a member is a member of their Team and every Team above it;
// a Privileged root is no Team.
func TestPersonTeamsWithDelegationOff(t *testing.T) {
	t.Parallel()
	m, ids := teamsRealm(t)
	d := identity.NewTeamDelegation(authz.DefaultPolicy(), m, m, identity.TeamDelegationOptions{})
	if d.On() {
		t.Fatal("the default policy has delegation on")
	}
	web := "WEBLAB=leader WEBLAB/BACKEND=leader WEBLAB/FRONTEND=leader WEBLAB/FRONTEND/REACT=leader WEBLAB/YK=leader"
	cases := []struct {
		name   string
		groups []string
		want   string
	}{
		{"top Leader", []string{"/UYELER/ARGE/WEBLAB/LIDERLER"}, web},
		{"coordinator", []string{"/UYELER/ARGE/WEBLAB/KOORDINATORLER"}, web},
		{"Leader and member of the same Team", []string{"/UYELER/ARGE/WEBLAB", "/UYELER/ARGE/WEBLAB/LIDERLER"}, web},
		{"Sub-team Leader", []string{"/UYELER/ARGE/WEBLAB/FRONTEND/LIDERLER"},
			"WEBLAB=member WEBLAB/FRONTEND=leader WEBLAB/FRONTEND/REACT=leader"},
		{"Sub-team coordinator", []string{"/UYELER/ARGE/WEBLAB/BACKEND/KOORDINATORLER"},
			"WEBLAB=member WEBLAB/BACKEND=leader"},
		{"a Team and a Sub-team of it", []string{"/UYELER/ARGE/WEBLAB/FRONTEND/REACT/LIDERLER", "/UYELER/ARGE/WEBLAB/LIDERLER"}, web},
		{"two Teams", []string{"/UYELER/ARGE/MOBILAB/LIDERLER", "/UYELER/ARGE/WEBLAB/BACKEND/KOORDINATORLER"},
			"MOBILAB=leader WEBLAB=member WEBLAB/BACKEND=leader"},
		{"member", []string{"/UYELER/ARGE/WEBLAB"}, "WEBLAB=member"},
		{"Sub-team member", []string{"/UYELER/ARGE/WEBLAB/FRONTEND/REACT"},
			"WEBLAB=member WEBLAB/FRONTEND=member WEBLAB/FRONTEND/REACT=member"},
		{"member here, Leader there", []string{"/UYELER/ARGE/MOBILAB", "/UYELER/ARGE/WEBLAB/BACKEND/KOORDINATORLER"},
			"MOBILAB=member WEBLAB=member WEBLAB/BACKEND=leader"},
		{"a Privileged root and its Leader group", []string{"/UYELER/YK", "/UYELER/YK/LIDERLER"}, ""},
		{"an area", []string{"/UYELER/ARGE"}, ""},
		{"nobody", nil, ""},
	}
	for _, c := range cases {
		groups := make([]string, 0, len(c.groups))
		for _, path := range c.groups {
			groups = append(groups, ids[path])
		}
		teams, err := d.PersonTeams(context.Background(), teamsPerson(t, m, groups...))
		if err != nil || teams == nil || renderTeams(teams) != c.want {
			t.Errorf("%s: %q %v, want %q", c.name, renderTeams(teams), err, c.want)
		}
	}

	person := teamsPerson(t, m, ids["/UYELER/ARGE/WEBLAB/FRONTEND/LIDERLER"])
	teams, err := d.PersonTeams(context.Background(), person)
	if err != nil || len(teams) != 3 {
		t.Fatalf("teams %+v %v", teams, err)
	}
	if front := teams[1]; front.ID != ids["/UYELER/ARGE/WEBLAB/FRONTEND"] || front.Code != "FRONTEND" || front.DisplayName.TR != "Ad FRONTEND" || front.Path != "/UYELER/ARGE/WEBLAB/FRONTEND" {
		t.Fatalf("front %+v", front)
	}
	// No address, no other person, no count: only the Teams.
	raw, err := json.Marshal(teams)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"@", "Count", "email"} {
		if strings.Contains(string(raw), field) {
			t.Fatalf("%q in %s", field, raw)
		}
	}
	// No managed tree and no capabilities' managed Teams with delegation
	// off: reading a person's Teams opens neither.
	if tree, err := d.Tree(context.Background(), person, false); err != nil || len(tree) != 0 {
		t.Fatalf("managed tree %+v %v", tree, err)
	}
	if managed, _, err := d.ManagedTeams(context.Background(), person); err != nil || len(managed) != 0 {
		t.Fatalf("managed teams %+v %v", managed, err)
	}
}

// The answer is kept a minute and dropped by Forget; a Keycloak that cannot
// be read is an error, never a partial answer, and is not kept.
func TestPersonTeamsCachesAndFails(t *testing.T) {
	t.Parallel()
	m, ids := teamsRealm(t)
	dir := &failingGroups{Memory: m}
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	d := identity.NewTeamDelegation(authz.DefaultPolicy(), dir, m, identity.TeamDelegationOptions{Now: clock})
	person := teamsPerson(t, m, ids["/UYELER/ARGE/MOBILAB/LIDERLER"])

	if teams, err := d.PersonTeams(context.Background(), person); err != nil || renderTeams(teams) != "MOBILAB=leader" {
		t.Fatalf("first: %q %v", renderTeams(teams), err)
	}
	dir.down = true
	if teams, err := d.PersonTeams(context.Background(), person); err != nil || renderTeams(teams) != "MOBILAB=leader" {
		t.Fatalf("kept: %q %v", renderTeams(teams), err)
	}
	now = now.Add(61 * time.Second)
	if teams, err := d.PersonTeams(context.Background(), person); err == nil || teams != nil {
		t.Fatalf("down: %+v %v", teams, err)
	}
	reads := dir.reads
	if _, err := d.PersonTeams(context.Background(), person); err == nil || dir.reads == reads {
		t.Fatalf("a failure is kept: %v, reads %d→%d", err, reads, dir.reads)
	}
	dir.down = false
	if teams, err := d.PersonTeams(context.Background(), person); err != nil || renderTeams(teams) != "MOBILAB=leader" {
		t.Fatalf("back: %q %v", renderTeams(teams), err)
	}
	// A change made outside core is seen after the minute; Forget (core's
	// own membership write) drops the answer at once.
	if err := m.RemoveMember(context.Background(), ids["/UYELER/ARGE/MOBILAB/LIDERLER"], person); err != nil {
		t.Fatal(err)
	}
	if teams, err := d.PersonTeams(context.Background(), person); err != nil || len(teams) != 1 {
		t.Fatalf("kept after a Keycloak change: %q %v", renderTeams(teams), err)
	}
	d.Forget(person)
	if teams, err := d.PersonTeams(context.Background(), person); err != nil || len(teams) != 0 {
		t.Fatalf("after Forget: %q %v", renderTeams(teams), err)
	}

	var none *identity.TeamDelegation
	if _, err := none.PersonTeams(context.Background(), person); err == nil {
		t.Fatal("a nil delegation answers")
	}
}

// A Leader group whose path only looks like one under a Team (a "/" in a
// name) makes its member neither Leader nor member of that Team.
func TestPersonTeamsNeedsTheLeaderGroupUnderItsTeam(t *testing.T) {
	t.Parallel()
	m, ids := teamsRealm(t)
	fake := uuid.NewString()
	m.PutGroup(identity.Group{ID: fake, Name: "X/LIDERLER", Path: "/UYELER/ARGE/MOBILAB/X/LIDERLER"})
	m.SetGroupParent(fake, ids["/UYELER/ARGE/MOBILAB"])
	d := identity.NewTeamDelegation(authz.DefaultPolicy(), m, m, identity.TeamDelegationOptions{})
	if teams, err := d.PersonTeams(context.Background(), teamsPerson(t, m, fake)); err != nil || len(teams) != 0 {
		t.Fatalf("fake Leader group: %q %v", renderTeams(teams), err)
	}
}

// brokenReads is a directory whose read of one Group by id (GroupNode) or
// of the Groups right under one (Subgroups) fails.
type brokenReads struct {
	*identity.Memory
	node, subgroups string
}

func (b brokenReads) GroupNode(ctx context.Context, id string) (identity.Group, string, error) {
	if id == b.node {
		return identity.Group{}, "", errors.New("keycloak: group read failed with status 502")
	}
	return b.Memory.GroupNode(ctx, id)
}

func (b brokenReads) Subgroups(ctx context.Context, id string) ([]identity.Group, error) {
	if id == b.subgroups {
		return nil, errors.New("keycloak: subgroups read failed with status 502")
	}
	return b.Memory.Subgroups(ctx, id)
}

// A read that fails halfway (a Sub-team, the Groups under one, a Team above
// a member's, the Leader group itself) is an error and no Team at all,
// never the part read before it; a later answer is read again.
func TestPersonTeamsHalfReadIsNoAnswer(t *testing.T) {
	t.Parallel()
	m, ids := teamsRealm(t)
	leader := teamsPerson(t, m, ids["/UYELER/ARGE/WEBLAB/LIDERLER"], ids["/UYELER/ARGE/MOBILAB"])
	member := teamsPerson(t, m, ids["/UYELER/ARGE/WEBLAB/FRONTEND/REACT"])
	for name, c := range map[string]struct {
		broken brokenReads
		person uuid.UUID
	}{
		"a Sub-team by id":            {brokenReads{Memory: m, node: ids["/UYELER/ARGE/WEBLAB/FRONTEND/REACT"]}, leader},
		"the Groups under a Sub-team": {brokenReads{Memory: m, subgroups: ids["/UYELER/ARGE/WEBLAB/FRONTEND"]}, leader},
		"the led Team itself":         {brokenReads{Memory: m, node: ids["/UYELER/ARGE/WEBLAB"]}, leader},
		"the Leader group":            {brokenReads{Memory: m, node: ids["/UYELER/ARGE/WEBLAB/LIDERLER"]}, leader},
		"a member's Team":             {brokenReads{Memory: m, node: ids["/UYELER/ARGE/MOBILAB"]}, leader},
		"a Team above a member's":     {brokenReads{Memory: m, node: ids["/UYELER/ARGE/WEBLAB/FRONTEND"]}, member},
	} {
		d := identity.NewTeamDelegation(authz.DefaultPolicy(), c.broken, c.broken, identity.TeamDelegationOptions{})
		teams, err := d.PersonTeams(context.Background(), c.person)
		if err == nil || teams != nil {
			t.Errorf("%s: %q %v, want an error and no Teams", name, renderTeams(teams), err)
			continue
		}
		// The failure is not kept: the next question reads again.
		if _, again := d.PersonTeams(context.Background(), c.person); again == nil {
			t.Errorf("%s: the second question did not read again", name)
		}
	}
}
