package identity_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/identity"
)

// failingGroups is a directory whose reads of a person's Groups fail while
// down is set, counting them.
type failingGroups struct {
	*identity.Memory
	mu    sync.Mutex
	down  bool
	reads int
}

func (f *failingGroups) GroupsForUser(ctx context.Context, id uuid.UUID) ([]identity.Group, error) {
	f.mu.Lock()
	f.reads++
	down := f.down
	f.mu.Unlock()
	if down {
		return nil, errors.New("keycloak: unavailable")
	}
	return f.Memory.GroupsForUser(ctx, id)
}

func delegationRealm(t *testing.T) (*identity.Memory, uuid.UUID, string) {
	t.Helper()
	m := identity.NewMemory()
	ids := map[string]string{}
	for _, path := range []string{"/UYELER", "/UYELER/ARGE", "/UYELER/ARGE/WEBLAB", "/UYELER/ARGE/WEBLAB/LIDERLER"} {
		id := uuid.NewString()
		ids[path] = id
		name := path[len(path)-len(lastSegment(path)):]
		m.PutGroup(identity.Group{ID: id, Name: name, Path: path, Attributes: map[string]string{"display_name_tr": "Ad " + name}})
	}
	leader := uuid.New()
	m.PutUser(identity.Person{ID: leader, Email: "l@example.test"})
	if err := m.AddMember(context.Background(), ids["/UYELER/ARGE/WEBLAB/LIDERLER"], leader); err != nil {
		t.Fatal(err)
	}
	return m, leader, ids["/UYELER/ARGE/WEBLAB"]
}

func lastSegment(path string) string {
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '/' {
			return path[i+1:]
		}
	}
	return path
}

func onPolicy() authz.Policy {
	p := authz.DefaultPolicy()
	p.TeamDelegation = true
	return p
}

// The capabilities' managed Teams: the Teams the person leads, kept a
// minute; a Keycloak that cannot be read answers none with the error and
// is asked again ten seconds on; off answers none and reads nothing.
func TestManagedTeamsCachesAndFails(t *testing.T) {
	t.Parallel()
	m, leader, weblab := delegationRealm(t)
	dir := &failingGroups{Memory: m}
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	d := identity.NewTeamDelegation(onPolicy(), dir, m, identity.TeamDelegationOptions{Now: clock})

	teams, cached, err := d.ManagedTeams(context.Background(), leader)
	if err != nil || cached || len(teams) != 1 || teams[0].ID != weblab || teams[0].Code != "WEBLAB" || teams[0].DisplayName.TR != "Ad WEBLAB" {
		t.Fatalf("teams %+v cached %v err %v", teams, cached, err)
	}
	dir.down = true
	if teams, cached, err := d.ManagedTeams(context.Background(), leader); err != nil || !cached || len(teams) != 1 {
		t.Fatalf("kept: %+v %v %v", teams, cached, err)
	}
	now = now.Add(61 * time.Second)
	teams, _, err = d.ManagedTeams(context.Background(), leader)
	if err == nil || len(teams) != 0 {
		t.Fatalf("down: %+v %v", teams, err)
	}
	reads := dir.reads
	now = now.Add(5 * time.Second)
	if _, cached, _ := d.ManagedTeams(context.Background(), leader); !cached || dir.reads != reads {
		t.Fatalf("a failure is kept: cached %v reads %d→%d", cached, reads, dir.reads)
	}
	dir.down = false
	now = now.Add(6 * time.Second)
	if teams, _, err := d.ManagedTeams(context.Background(), leader); err != nil || len(teams) != 1 {
		t.Fatalf("back: %+v %v", teams, err)
	}

	off := identity.NewTeamDelegation(authz.DefaultPolicy(), dir, m, identity.TeamDelegationOptions{})
	reads = dir.reads
	if teams, _, err := off.ManagedTeams(context.Background(), leader); err != nil || len(teams) != 0 || dir.reads != reads {
		t.Fatalf("off: %+v %v, reads %d→%d", teams, err, reads, dir.reads)
	}
	var none *identity.TeamDelegation
	if none.On() {
		t.Fatal("a nil delegation is on")
	}
	none.Forget(leader)
	none.ForgetAll()
}

// A Leader group answers for the Team its id names only when its path is
// that Team's: a Group named LIDERLER moved under another Team's path by a
// "/" in a name grants nothing.
func TestManagedTeamsNeedsTheLeaderGroupUnderItsTeam(t *testing.T) {
	t.Parallel()
	m, leader, _ := delegationRealm(t)
	other := uuid.NewString()
	m.PutGroup(identity.Group{ID: other, Name: "MOBILAB", Path: "/UYELER/ARGE/MOBILAB"})
	fake := uuid.NewString()
	m.PutGroup(identity.Group{ID: fake, Name: "X/LIDERLER", Path: "/UYELER/ARGE/MOBILAB/X/LIDERLER"})
	m.SetGroupParent(fake, other)
	if err := m.AddMember(context.Background(), fake, leader); err != nil {
		t.Fatal(err)
	}
	d := identity.NewTeamDelegation(onPolicy(), m, m, identity.TeamDelegationOptions{})
	teams, _, err := d.ManagedTeams(context.Background(), leader)
	if err != nil || len(teams) != 1 || teams[0].Code != "WEBLAB" {
		t.Fatalf("teams %+v %v", teams, err)
	}
}

func TestTeamDelegationOptionsFromEnv(t *testing.T) {
	t.Parallel()
	env := func(values map[string]string) func(string) string { return func(k string) string { return values[k] } }
	got, err := identity.TeamDelegationOptionsFromEnv(env(nil))
	if err != nil || got.GroupLimit != 0 || got.WritesPerMinute != 0 {
		t.Fatalf("unset: %+v %v", got, err)
	}
	got, err = identity.TeamDelegationOptionsFromEnv(env(map[string]string{identity.DelegatedGroupLimitEnv: "20", identity.DelegatedWritesEnv: " 30 "}))
	if err != nil || got.GroupLimit != 20 || got.WritesPerMinute != 30 {
		t.Fatalf("set: %+v %v", got, err)
	}
	for _, raw := range []string{"0", "-1", "x", "1.5"} {
		if _, err := identity.TeamDelegationOptionsFromEnv(env(map[string]string{identity.DelegatedWritesEnv: raw})); err == nil {
			t.Errorf("%q accepted", raw)
		}
	}
}

func TestMaskEmail(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"ada@example.com":   "a***@example.com",
		"Ş@x.tr":            "Ş***@x.tr",
		"no-at-sign":        "***",
		"@example.com":      "***",
		" ada@example.com ": "a***@example.com",
	} {
		if got := identity.MaskEmail(in); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
}

// The memory directory's composites are opened as Keycloak's are: eight
// levels of app roles protect nothing, a deeper chain protects (the limit
// is the safe answer), a realm role inside one protects.
func TestMemoryGroupProtectionDepth(t *testing.T) {
	t.Parallel()
	app := authz.AppRoles()
	level := func(i int) identity.ClientRole {
		return identity.ClientRole{ClientID: app[i].Client, Role: app[i].Role}
	}
	chain := func(n int) *identity.Memory {
		m := identity.NewMemory()
		m.PutGroup(identity.Group{ID: "g", Name: "G", Path: "/G"})
		for i := 0; i < n; i++ {
			m.PutCompositeRole(level(i), level(i+1))
		}
		if err := m.SetGroupClientRoles(context.Background(), "g", []identity.ClientRole{level(0)}); err != nil {
			t.Fatal(err)
		}
		return m
	}
	for n, want := range map[int]bool{1: false, 8: false, 9: true, 11: true} {
		got, err := chain(n).GroupProtection(context.Background(), "g")
		if err != nil || got.Protected != want {
			t.Errorf("%d levels: %+v %v", n, got, err)
		}
	}
	// A realm role three deep comes back by name for the delegation to
	// class; it does not protect at this level.
	m := chain(3)
	m.PutCompositeRole(level(3), identity.ClientRole{Role: "admin"})
	if got, _ := m.GroupProtection(context.Background(), "g"); got.Protected || !slices.Contains(got.RealmRoles, "admin") {
		t.Errorf("a realm role three deep: %+v", got)
	}
}

// Roles are classed by their (client, role) pair (ADR 0065 decision 10,
// addendum of 2026-10-12): an app role protects nothing, an admin role and
// a role nobody has classified protect, a composite app role protects when
// one role inside it is no app role.
func TestMemoryGroupProtectionRoleClasses(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name         string
		roles        []identity.ClientRole
		composites   map[identity.ClientRole][]identity.ClientRole
		want         bool
		unclassified string
	}{
		{name: "an app role", roles: []identity.ClientRole{{ClientID: "core", Role: "certificate:issue"}}},
		{name: "app roles of three clients", roles: []identity.ClientRole{{ClientID: "forms", Role: "skyforms:access"}, {ClientID: "skymail", Role: "skymail:access"}, {ClientID: "frontend-arge", Role: "content:read"}}},
		{name: "an admin role", roles: []identity.ClientRole{{ClientID: "skymail", Role: "skymail:system-mails:read"}}, want: true},
		{name: "cms:access by its name", roles: []identity.ClientRole{{ClientID: "frontend-main", Role: "cms:access"}}, want: true},
		{name: "an unclassified role", roles: []identity.ClientRole{{ClientID: "forms", Role: "forms:editor"}}, want: true, unclassified: "forms:forms:editor"},
		{name: "an app role's name on another client", roles: []identity.ClientRole{{ClientID: "skymail", Role: "certificate:issue"}}, want: true, unclassified: "skymail:certificate:issue"},
		{name: "a composite app role holding an admin role", roles: []identity.ClientRole{{ClientID: "core", Role: "certificate:issue"}},
			composites: map[identity.ClientRole][]identity.ClientRole{{ClientID: "core", Role: "certificate:issue"}: {{ClientID: "core", Role: "users:read"}}}, want: true},
		{name: "a composite app role holding an unclassified role", roles: []identity.ClientRole{{ClientID: "core", Role: "certificate:issue"}},
			composites: map[identity.ClientRole][]identity.ClientRole{{ClientID: "core", Role: "certificate:issue"}: {{ClientID: "place", Role: "place:new"}}}, want: true, unclassified: "place:place:new"},
		{name: "a composite app role holding app roles", roles: []identity.ClientRole{{ClientID: "core", Role: "certificate:issue"}},
			composites: map[identity.ClientRole][]identity.ClientRole{{ClientID: "core", Role: "certificate:issue"}: {{ClientID: "core", Role: "certificate:revoke"}, {ClientID: "frontend-main", Role: "content:read"}}}},
	} {
		m := identity.NewMemory()
		m.PutGroup(identity.Group{ID: "g", Name: "G", Path: "/G"})
		for parent, children := range tc.composites {
			m.PutCompositeRole(parent, children...)
		}
		if err := m.SetGroupClientRoles(context.Background(), "g", tc.roles); err != nil {
			t.Fatal(err)
		}
		got, err := m.GroupProtection(context.Background(), "g")
		if err != nil || got.Protected != tc.want || got.Unclassified != tc.unclassified || (got.Protected && got.Reason == "") {
			t.Errorf("%s: %+v %v", tc.name, got, err)
		}
	}
}

// A protection read that finds a role nobody has classified logs it and
// counts it on /v1/metrics by its (client, role) pair, never naming a
// person; a known admin role is neither logged as unclassified nor counted.
func TestTeamDelegationCountsUnclassifiedRoles(t *testing.T) {
	t.Parallel()
	m, leader, weblab := delegationRealm(t)
	if err := m.SetGroupClientRoles(context.Background(), weblab, []identity.ClientRole{{ClientID: "forms", Role: "forms:editor"}}); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var lines []string
	logf := func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		lines = append(lines, fmt.Sprintf(format, args...))
	}
	d := identity.NewTeamDelegation(onPolicy(), m, m, identity.TeamDelegationOptions{Logf: logf})
	tree, err := d.Tree(context.Background(), leader, false)
	if err != nil || len(tree) != 1 || !tree[0].Protected {
		t.Fatalf("tree %+v %v", tree, err)
	}
	if got := d.Prometheus(); !strings.Contains(got, `skylab_team_delegation_unclassified_roles_total{role="forms:forms:editor"} 1`) {
		t.Fatalf("metrics:\n%s", got)
	}
	mu.Lock()
	logged := strings.Join(lines, "\n")
	mu.Unlock()
	if !strings.Contains(logged, "client role forms:forms:editor is unclassified") || strings.Contains(logged, "l@example.test") || strings.Contains(logged, leader.String()) {
		t.Fatalf("log:\n%s", logged)
	}

	known, leader2, weblab2 := delegationRealm(t)
	if err := known.SetGroupClientRoles(context.Background(), weblab2, []identity.ClientRole{{ClientID: "skymail", Role: "skymail:lists:read"}}); err != nil {
		t.Fatal(err)
	}
	d2 := identity.NewTeamDelegation(onPolicy(), known, known, identity.TeamDelegationOptions{})
	if tree, err := d2.Tree(context.Background(), leader2, false); err != nil || len(tree) != 1 || !tree[0].Protected {
		t.Fatalf("admin role: %+v %v", tree, err)
	}
	if got := d2.Prometheus(); strings.Contains(got, "{role=") {
		t.Fatalf("a known admin role counted:\n%s", got)
	}
	// A realm role nobody has classified is counted as realm:<name>.
	realm, leader3, weblab3 := delegationRealm(t)
	if err := realm.SetGroupClientRoles(context.Background(), weblab3, []identity.ClientRole{{Role: "offline_access"}}); err != nil {
		t.Fatal(err)
	}
	d3 := identity.NewTeamDelegation(onPolicy(), realm, realm, identity.TeamDelegationOptions{})
	if tree, err := d3.Tree(context.Background(), leader3, false); err != nil || len(tree) != 1 || !tree[0].Protected {
		t.Fatalf("realm role: %+v %v", tree, err)
	}
	if got := d3.Prometheus(); !strings.Contains(got, `skylab_team_delegation_unclassified_roles_total{role="realm:offline_access"} 1`) {
		t.Fatalf("realm metrics:\n%s", got)
	}
	var none *identity.TeamDelegation
	if none.Prometheus() != "" {
		t.Fatal("a nil delegation renders metrics")
	}
}
