package commpref

import (
	"testing"

	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/identity"
)

// The tree of the tests: two areas, teams with sub-teams, a leader
// subgroup, and a Privileged root under /UYELER.
//
//	/UYELER
//	/UYELER/ARGE
//	/UYELER/ARGE/WEBLAB            (public)
//	/UYELER/ARGE/WEBLAB/FRONT
//	/UYELER/ARGE/WEBLAB/BACK
//	/UYELER/ARGE/WEBLAB/LIDERLER   (leader subgroup, not a team)
//	/UYELER/ARGE/AIRLAB            (public)
//	/UYELER/ARGE/AIRLAB/DRONE
//	/UYELER/ARGE/GIZLI             (hidden)
//	/UYELER/YK                     (Privileged root, not a team)
//	/UYELER/YK/ALT
func testGroups() []identity.Group {
	public := map[string]string{"public_listing": "true"}
	return []identity.Group{
		{ID: "g-uyeler", Name: "UYELER", Path: "/UYELER"},
		{ID: "g-arge", Name: "ARGE", Path: "/UYELER/ARGE"},
		{ID: "g-weblab", Name: "WEBLAB", Path: "/UYELER/ARGE/WEBLAB", Attributes: map[string]string{"public_listing": "true", "display_name_tr": "Web Geliştirme"}},
		{ID: "g-front", Name: "FRONT", Path: "/UYELER/ARGE/WEBLAB/FRONT"},
		{ID: "g-back", Name: "BACK", Path: "/UYELER/ARGE/WEBLAB/BACK"},
		{ID: "g-weblab-l", Name: "LIDERLER", Path: "/UYELER/ARGE/WEBLAB/LIDERLER"},
		{ID: "g-airlab", Name: "AIRLAB", Path: "/UYELER/ARGE/AIRLAB", Attributes: public},
		{ID: "g-drone", Name: "DRONE", Path: "/UYELER/ARGE/AIRLAB/DRONE"},
		{ID: "g-gizli", Name: "GIZLI", Path: "/UYELER/ARGE/GIZLI"},
		{ID: "g-yk", Name: "YK", Path: "/UYELER/YK"},
		{ID: "g-yk-alt", Name: "ALT", Path: "/UYELER/YK/ALT"},
	}
}

func testTree() *Tree { return NewTree(testGroups(), authz.DefaultPrivilegedRoots()) }

func TestTreeHoldsTeamsOnly(t *testing.T) {
	tree := testTree()
	for _, id := range []string{"g-weblab", "g-front", "g-back", "g-airlab", "g-drone", "g-gizli"} {
		if _, ok := tree.Team(id); !ok {
			t.Fatalf("%s is not a team", id)
		}
	}
	for _, id := range []string{"g-uyeler", "g-arge", "g-weblab-l", "g-yk", "g-yk-alt", "missing"} {
		if _, ok := tree.Team(id); ok {
			t.Fatalf("%s is a team", id)
		}
	}
	front, _ := tree.Team("g-front")
	if front.ParentID != "g-weblab" || front.Code != "FRONT" || front.DisplayName != "FRONT" {
		t.Fatalf("front = %+v", front)
	}
	weblab, _ := tree.Team("g-weblab")
	if weblab.ParentID != "" || !weblab.Public || weblab.DisplayName != "Web Geliştirme" || len(weblab.Children) != 2 {
		t.Fatalf("weblab = %+v", weblab)
	}
}

// Membership is TeamOf's: a sub-team's member is a member of every team
// above it, a leader subgroup's member of its team; a Privileged subtree
// and an area make no team member, but a Member all the same.
func TestMembershipFollowsTeamOf(t *testing.T) {
	tree := testTree()
	front := tree.Membership([]string{"/UYELER/ARGE/WEBLAB/FRONT"})
	if !front.Member || !front.Teams["g-front"] || !front.Teams["g-weblab"] || front.Teams["g-back"] || len(front.Teams) != 2 {
		t.Fatalf("front member = %+v", front)
	}
	leader := tree.Membership([]string{"/UYELER/ARGE/WEBLAB/LIDERLER"})
	if !leader.Teams["g-weblab"] || leader.Teams["g-front"] || len(leader.Teams) != 1 {
		t.Fatalf("leader = %+v", leader)
	}
	yk := tree.Membership([]string{"/UYELER/YK/ALT"})
	if !yk.Member || len(yk.Teams) != 0 {
		t.Fatalf("yk = %+v", yk)
	}
	outsider := tree.Membership([]string{"/ETKINLIK/GONULLU"})
	if outsider.Member || len(outsider.Teams) != 0 {
		t.Fatalf("outsider = %+v", outsider)
	}
}

func on(v bool) *bool { return &v }

// Defaults: club on for a Member; own teams, the teams above them and
// every team below those on (Y1: a WEBLAB member, by FRONT, gets BACK's
// mail too); any other team off; nothing at all for someone outside
// /UYELER.
func TestDefaults(t *testing.T) {
	tree := testTree()
	m := tree.Membership([]string{"/UYELER/ARGE/WEBLAB/FRONT"})
	for _, tc := range []struct {
		category Category
		want     bool
		from     string
	}{
		{Master(), true, ""},
		{Club(KindAnnouncement), true, ""},
		{Club(KindNews), true, ""},
		{Team("g-front", KindAnnouncement), true, ""},
		{Team("g-weblab", KindNews), true, ""},
		{Team("g-back", KindAnnouncement), true, "g-weblab"},
		{Team("g-airlab", KindAnnouncement), false, ""},
		{Team("g-drone", KindNews), false, ""},
		{Team("g-gizli", KindNews), false, ""},
		{Team("g-deleted", KindNews), false, ""},
	} {
		v := tree.Resolve(nil, m, tc.category)
		if v.Enabled != tc.want || v.Effective != tc.want || v.Origin != OriginDefault || v.From != tc.from {
			t.Fatalf("%s = %+v, want %v from %q", tc.category, v, tc.want, tc.from)
		}
	}
	// A member of the team itself gets every sub-team.
	weblab := tree.Membership([]string{"/UYELER/ARGE/WEBLAB"})
	for _, id := range []string{"g-weblab", "g-front", "g-back"} {
		if v := tree.Resolve(nil, weblab, Team(id, KindAnnouncement)); !v.Effective {
			t.Fatalf("WEBLAB member, %s = %+v", id, v)
		}
	}
	outsider := tree.Membership(nil)
	if v := tree.Resolve(nil, outsider, Club(KindAnnouncement)); v.Enabled {
		t.Fatalf("club for a non-member = %+v", v)
	}
}

// D5: a setting on a team covers its sub-teams; the most specific one wins,
// and an explicit choice above beats a default. A sub-team turned off
// stays off while its siblings follow the parent.
func TestInheritanceMostSpecificWins(t *testing.T) {
	tree := testTree()
	m := tree.Membership([]string{"/UYELER/ARGE/WEBLAB/FRONT"})
	explicit := Explicit{
		Team("g-airlab", KindAnnouncement): true,  // follows AIRLAB
		Team("g-weblab", KindAnnouncement): true,  // own parent, said explicitly
		Team("g-front", KindAnnouncement):  false, // but not FRONT
	}
	cases := []struct {
		category Category
		enabled  bool
		origin   Origin
		from     string
	}{
		{Team("g-front", KindAnnouncement), false, OriginExplicit, ""},
		{Team("g-back", KindAnnouncement), true, OriginInherited, "g-weblab"},
		{Team("g-weblab", KindAnnouncement), true, OriginExplicit, ""},
		{Team("g-drone", KindAnnouncement), true, OriginInherited, "g-airlab"},
		{Team("g-drone", KindNews), false, OriginDefault, ""}, // the kinds are separate (D6)
		{Team("g-front", KindNews), true, OriginDefault, ""},
		{Team("g-back", KindNews), true, OriginDefault, "g-weblab"},
	}
	for _, tc := range cases {
		v := tree.Resolve(explicit, m, tc.category)
		if v.Enabled != tc.enabled || v.Origin != tc.origin || v.From != tc.from || v.Effective != tc.enabled {
			t.Fatalf("%s = %+v, want %v %s %q", tc.category, v, tc.enabled, tc.origin, tc.from)
		}
	}
}

// A moved sub-team takes its new parent's setting and keeps its own.
func TestMovedSubTeamTakesTheNewParentsSetting(t *testing.T) {
	groups := testGroups()
	m := Membership{Member: true, Teams: map[string]bool{}}
	explicit := Explicit{
		Team("g-weblab", KindAnnouncement): true,
		Team("g-airlab", KindAnnouncement): false,
		Team("g-back", KindNews):           true,
	}
	before := NewTree(groups, authz.DefaultPrivilegedRoots())
	if v := before.Resolve(explicit, m, Team("g-back", KindAnnouncement)); !v.Enabled || v.From != "g-weblab" {
		t.Fatalf("before = %+v", v)
	}
	for i := range groups {
		if groups[i].ID == "g-back" {
			groups[i].Path = "/UYELER/ARGE/AIRLAB/BACK"
		}
	}
	after := NewTree(groups, authz.DefaultPrivilegedRoots())
	if v := after.Resolve(explicit, m, Team("g-back", KindAnnouncement)); v.Enabled || v.From != "g-airlab" || v.Origin != OriginInherited {
		t.Fatalf("after move = %+v", v)
	}
	if v := after.Resolve(explicit, m, Team("g-back", KindNews)); !v.Enabled || v.Origin != OriginExplicit {
		t.Fatalf("own setting after move = %+v", v)
	}
}

// The master switch off turns everything off without touching the rows; on
// again, every earlier setting is back.
func TestMasterSwitchOverridesAndKeepsTheSettings(t *testing.T) {
	tree := testTree()
	m := tree.Membership([]string{"/UYELER/ARGE/WEBLAB/FRONT"})
	explicit := Explicit{
		Team("g-airlab", KindNews):         true,
		Club(KindAnnouncement):             false,
		Team("g-front", KindNews):          false,
		Team("g-weblab", KindNews):         true,
		Team("g-drone", KindNews):          false,
		Team("g-back", KindNews):           true,
		Team("g-gizli", KindNews):          true,
		Team("g-weblab", KindAnnouncement): true,
	}
	categories := []Category{Club(KindAnnouncement), Club(KindNews), Team("g-airlab", KindNews), Team("g-front", KindNews), Team("g-front", KindAnnouncement), Team("g-drone", KindNews)}
	want := map[Category]bool{}
	for _, c := range categories {
		want[c] = tree.Resolve(explicit, m, c).Effective
	}
	explicit[Master()] = false
	for _, c := range categories {
		v := tree.Resolve(explicit, m, c)
		if v.Effective || v.Enabled != want[c] {
			t.Fatalf("master off: %s = %+v (enabled want %v)", c, v, want[c])
		}
	}
	if v := tree.Resolve(explicit, m, Master()); v.Enabled || v.Origin != OriginExplicit {
		t.Fatalf("master = %+v", v)
	}
	explicit[Master()] = true
	for _, c := range categories {
		if v := tree.Resolve(explicit, m, c); v.Effective != want[c] {
			t.Fatalf("master on again: %s = %+v, want %v", c, v, want[c])
		}
	}
}

func TestCategoryStringsRoundTrip(t *testing.T) {
	for _, c := range []Category{Master(), Club(KindAnnouncement), Club(KindNews), Team("2b0f8a4e-0f4c-4a43-9b0b-1d1e2f3a4b5c", KindNews)} {
		parsed, err := ParseCategory(c.String())
		if err != nil || parsed != c {
			t.Fatalf("%s -> %+v %v", c, parsed, err)
		}
	}
	for _, bad := range []string{"", "all:duyuru", "club", "club:bulten", "team::duyuru", "team:g", "team:g:duyuru:x", "team:a b:haber", "team:" + string(make([]byte, 65)) + ":haber", "kulup:duyuru"} {
		if _, err := ParseCategory(bad); err == nil {
			t.Fatalf("%q parsed", bad)
		}
	}
}

// Y1: a member subscribed to their team by default gets its sub-teams too
// and may turn one off on its own; the others and the team stay on.
func TestDefaultSubscriberTurnsOneSubTeamOff(t *testing.T) {
	tree := testTree()
	m := tree.Membership([]string{"/UYELER/ARGE/WEBLAB"})
	explicit := Explicit{Team("g-back", KindAnnouncement): false}
	for id, want := range map[string]bool{"g-back": false, "g-front": true, "g-weblab": true} {
		if v := tree.Resolve(explicit, m, Team(id, KindAnnouncement)); v.Effective != want {
			t.Fatalf("%s = %+v, want %v", id, v, want)
		}
	}
	if v := tree.Resolve(explicit, m, Team("g-back", KindNews)); !v.Effective {
		t.Fatalf("the other kind of BACK = %+v", v)
	}
}
