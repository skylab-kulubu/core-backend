package authz

import (
	"slices"
	"strings"
	"testing"
)

// fakeTeamTree is the realm's Groups by path, as identity.TeamTree reads
// them; down says the tree could not be read.
type fakeTeamTree struct {
	paths []string
	down  bool
	asked []string
}

func (f *fakeTeamTree) TeamPath(team string) (string, bool) {
	f.asked = append(f.asked, team)
	if f.down {
		return "", false
	}
	var found []string
	for _, path := range f.paths {
		if strings.EqualFold(path[strings.LastIndex(path, "/")+1:], team) {
			found = append(found, path)
		}
	}
	if len(found) != 1 {
		return "", false
	}
	return found[0], true
}

func (f *fakeTeamTree) SubTeams(path string) []string {
	if f.down {
		return nil
	}
	var out []string
	for _, p := range f.paths {
		if strings.HasPrefix(p, path+"/") {
			out = append(out, p[strings.LastIndex(p, "/")+1:])
		}
	}
	return out
}

// realmTree is ARGE's Teams: WEBLAB with a Sub-team MOBIL and its own
// Sub-team IOS, a sibling Team GAMELAB, and ORGANIZASYON's GECEKODU.
func realmTree() *fakeTeamTree {
	return &fakeTeamTree{paths: []string{
		"/UYELER",
		"/UYELER/ARGE",
		"/UYELER/ARGE/KOORDINATORLER",
		"/UYELER/ARGE/WEBLAB",
		"/UYELER/ARGE/WEBLAB/LIDERLER",
		"/UYELER/ARGE/WEBLAB/MOBIL",
		"/UYELER/ARGE/WEBLAB/MOBIL/LIDERLER",
		"/UYELER/ARGE/WEBLAB/MOBIL/IOS",
		"/UYELER/ARGE/WEBLAB/MOBIL/IOS/LIDERLER",
		"/UYELER/ARGE/GAMELAB",
		"/UYELER/ARGE/GAMELAB/LIDERLER",
		"/UYELER/ORGANIZASYON",
		"/UYELER/ORGANIZASYON/GECEKODU",
		"/UYELER/ORGANIZASYON/GECEKODU/KOORDINATORLER",
		"/UYELER/ORGANIZASYON/GECEKODU/SAHNE",
		"/UYELER/YK",
		"/UYELER/YK/MOBIL2",
	}}
}

func inheritingAuthorizer(tree TeamTree) Authorizer {
	policy := DefaultPolicy()
	policy.TeamDelegation = true
	return NewAuthorizer(policy, WithTeamTree(tree))
}

// The Leader of a Team above an Event's Owner team is that team's Leader
// (ADR-0065 decision 2): the Event, its tickets and applications, its door,
// competitions and certificates.
func TestOwnerLevels_LeaderOfAnAncestorTeamLeadsTheSubTeam(t *testing.T) {
	t.Parallel()
	auth := inheritingAuthorizer(realmTree())
	weblabLeader := Principal{ID: "u1", Groups: []string{"/UYELER/ARGE/WEBLAB/LIDERLER"}}
	for _, owner := range []string{"MOBIL", "IOS"} {
		for _, c := range []struct {
			name string
			r    Resource
			a    Action
		}{
			{"event create", Resource{Type: TypeEvent, OwnerTeam: owner}, Create},
			{"event update", Resource{Type: TypeEvent, OwnerTeam: owner}, Update},
			{"event delete", Resource{Type: TypeEvent, OwnerTeam: owner}, Delete},
			{"event draft read", Resource{Type: TypeEvent, OwnerTeam: owner}, ReadFull},
			{"session update", Resource{Type: TypeSession, OwnerTeam: owner}, Update},
			{"applications read", Resource{Type: TypeTicket, OwnerTeam: owner}, Read},
			{"ticket assign", Resource{Type: TypeTicket, OwnerTeam: owner}, Assign},
			{"door check-in", Resource{Type: TypeTicket, OwnerTeam: owner}, Validate},
			{"competition update", Resource{Type: TypeCompetition, OwnerTeam: owner}, Update},
			{"competitor read", Resource{Type: TypeCompetitor, OwnerTeam: owner}, Read},
			{"certificate issue", Resource{Type: TypeCertificate, OwnerTeam: owner}, Issue},
			{"certificate template update", Resource{Type: TypeCertificateTemplate, OwnerTeam: owner}, Update},
		} {
			if !auth.Allow(weblabLeader, c.r, c.a) {
				t.Errorf("WEBLAB's Leader is refused %s of a %s Event", c.name, owner)
			}
		}
	}
	// KOORDINATORLER leads as LIDERLER does.
	gecekoduLeader := Principal{ID: "u2", Groups: []string{"/UYELER/ORGANIZASYON/GECEKODU/KOORDINATORLER"}}
	if !auth.Allow(gecekoduLeader, Resource{Type: TypeEvent, OwnerTeam: "SAHNE"}, Delete) {
		t.Error("GECEKODU's KOORDINATORLER is refused deleting a SAHNE Event")
	}
}

func TestOwnerLevels_InheritanceStopsAtTheSubtree(t *testing.T) {
	t.Parallel()
	auth := inheritingAuthorizer(realmTree())
	update := func(p Principal, owner string) bool {
		return auth.Allow(p, Resource{Type: TypeEvent, OwnerTeam: owner}, Update)
	}
	applications := func(p Principal, owner string) bool {
		return auth.Allow(p, Resource{Type: TypeTicket, OwnerTeam: owner}, Read)
	}
	gamelabLeader := Principal{ID: "u1", Groups: []string{"/UYELER/ARGE/GAMELAB/LIDERLER"}}
	if update(gamelabLeader, "MOBIL") || applications(gamelabLeader, "MOBIL") {
		t.Error("a sibling Team's Leader manages a MOBIL Event")
	}
	if update(gamelabLeader, "WEBLAB") {
		t.Error("a sibling Team's Leader manages a WEBLAB Event")
	}
	// A Sub-team's Leader is only a Member of the Team above.
	mobilLeader := Principal{ID: "u2", Groups: []string{"/UYELER/ARGE/WEBLAB/MOBIL/LIDERLER"}}
	if got := auth.Capabilities(mobilLeader); !slices.Equal(levelsOf(got, "WEBLAB"), []Level{LevelMember}) {
		t.Errorf("MOBIL's Leader levels in WEBLAB = %v, want [MEMBER]", levelsOf(got, "WEBLAB"))
	}
	if update(mobilLeader, "WEBLAB") || applications(mobilLeader, "WEBLAB") {
		t.Error("a Sub-team's Leader manages the Team above")
	}
	if !update(mobilLeader, "IOS") {
		t.Error("MOBIL's Leader is refused its own Sub-team's Event")
	}
	// An area is no Team: its coordinators lead no Team below it.
	areaCoordinator := Principal{ID: "u3", Groups: []string{"/UYELER/ARGE/KOORDINATORLER"}}
	if update(areaCoordinator, "WEBLAB") || update(areaCoordinator, "MOBIL") {
		t.Error("an area's KOORDINATORLER leads a Team below the area")
	}
	// A plain member of the Team above gains nothing.
	weblabMember := Principal{ID: "u4", Groups: []string{"/UYELER/ARGE/WEBLAB"}}
	if update(weblabMember, "MOBIL") || applications(weblabMember, "MOBIL") {
		t.Error("a member of the Team above manages a Sub-team's Event")
	}
	// A Team under a Privileged root is no Owner team of a Leader.
	if update(Principal{ID: "u5", Groups: []string{"/UYELER/ARGE/WEBLAB/LIDERLER"}}, "MOBIL2") {
		t.Error("a Leader manages an Event of a Group under a Privileged root")
	}
}

// Without the tree, with delegation off, or when the name is not one
// Team's, the decision is today's: no inheritance.
func TestOwnerLevels_NoInheritanceWithoutAnAnswer(t *testing.T) {
	t.Parallel()
	weblabLeader := Principal{ID: "u1", Groups: []string{"/UYELER/ARGE/WEBLAB/LIDERLER"}}
	mobilEvent := Resource{Type: TypeEvent, OwnerTeam: "MOBIL"}

	if NewAuthorizer(DefaultPolicy()).Allow(weblabLeader, mobilEvent, Update) {
		t.Error("no tree: WEBLAB's Leader manages a MOBIL Event")
	}

	tree := realmTree()
	off := NewAuthorizer(DefaultPolicy(), WithTeamTree(tree))
	if off.Allow(weblabLeader, mobilEvent, Update) {
		t.Error("delegation off: WEBLAB's Leader manages a MOBIL Event")
	}
	if len(tree.asked) != 0 {
		t.Errorf("delegation off read the tree for %v", tree.asked)
	}

	down := realmTree()
	down.down = true
	if inheritingAuthorizer(down).Allow(weblabLeader, mobilEvent, Update) {
		t.Error("tree unread: WEBLAB's Leader manages a MOBIL Event")
	}

	twice := realmTree()
	twice.paths = append(twice.paths, "/UYELER/ORGANIZASYON/MOBIL")
	if inheritingAuthorizer(twice).Allow(weblabLeader, mobilEvent, Update) {
		t.Error("two Groups named MOBIL: WEBLAB's Leader manages a MOBIL Event")
	}

	// The tree answers a Group that is no Team: a Leader group's name.
	leaders := &fakeTeamTree{paths: []string{"/UYELER/ARGE/WEBLAB/X/LIDERLER"}}
	if inheritingAuthorizer(leaders).Allow(weblabLeader, Resource{Type: TypeEvent, OwnerTeam: "LIDERLER"}, Update) {
		t.Error("a Leader group's name reads as an Owner team")
	}
}

// Privileged decisions read first, and a caller who leads nothing never
// reads the tree.
func TestOwnerLevels_PrivilegedAndMembersUnchanged(t *testing.T) {
	t.Parallel()
	tree := realmTree()
	auth := inheritingAuthorizer(tree)
	yk := Principal{ID: "u1", Groups: []string{"/UYELER/YK"}}
	if !auth.Allow(yk, Resource{Type: TypeEvent, OwnerTeam: "MOBIL"}, Update) {
		t.Error("YK is refused a MOBIL Event")
	}
	member := Principal{ID: "u2", Groups: []string{"/UYELER/ARGE/WEBLAB/MOBIL"}}
	if auth.Allow(member, Resource{Type: TypeEvent, OwnerTeam: "MOBIL"}, Update) {
		t.Error("a MOBIL member manages a MOBIL Event")
	}
	if len(tree.asked) != 0 {
		t.Errorf("read the tree for %v, want no read", tree.asked)
	}
}

// Capabilities list the Sub-teams a Leader leads from above, with their
// decisions, so the panel finds them in teams.
func TestCapabilities_LeaderListsSubTeams(t *testing.T) {
	t.Parallel()
	auth := inheritingAuthorizer(realmTree())
	c := auth.Capabilities(Principal{ID: "u1", Groups: []string{"/UYELER/ARGE/WEBLAB/LIDERLER"}})
	for _, team := range []string{"MOBIL", "IOS"} {
		if !slices.Equal(levelsOf(c, team), []Level{LevelLeader}) {
			t.Errorf("levels in %s = %v, want [LEADER]", team, levelsOf(c, team))
		}
		if !scopeFor(c, team)["event.update"] {
			t.Errorf("%s lacks event.update", team)
		}
	}
	if levelsOf(c, "GAMELAB") != nil {
		t.Error("a sibling Team is listed")
	}
	var names []string
	for _, team := range c.Teams {
		names = append(names, team.Team)
	}
	if !slices.IsSorted(names) {
		t.Errorf("teams %v are not sorted", names)
	}

	off := NewAuthorizer(DefaultPolicy(), WithTeamTree(realmTree()))
	if levelsOf(off.Capabilities(Principal{ID: "u1", Groups: []string{"/UYELER/ARGE/WEBLAB/LIDERLER"}}), "MOBIL") != nil {
		t.Error("delegation off lists a Sub-team")
	}
}

func levelsOf(c Capabilities, team string) []Level {
	for _, t := range c.Teams {
		if t.Team == team {
			return t.Levels
		}
	}
	return nil
}
