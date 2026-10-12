package identity_test

import (
	"context"
	"errors"
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
