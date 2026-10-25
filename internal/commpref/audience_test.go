package commpref

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/identity"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// fakeKeycloak is the Keycloak of the audience tests: Groups, their direct
// members, and people with the flags core reads (enabled, emailVerified, a
// service account).
type fakeKeycloak struct {
	mu        sync.Mutex
	groups    []identity.Group
	members   map[string][]uuid.UUID
	people    map[uuid.UUID]identity.AudiencePerson
	listCalls int
	fail      error
}

func (k *fakeKeycloak) ListGroups(context.Context) ([]identity.Group, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.listCalls++
	if k.fail != nil {
		return nil, k.fail
	}
	return slices.Clone(k.groups), nil
}

func (k *fakeKeycloak) AudienceMembers(_ context.Context, groupID string) ([]identity.AudiencePerson, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	var out []identity.AudiencePerson
	for _, id := range k.members[groupID] {
		out = append(out, k.people[id])
	}
	return out, nil
}

// AudiencePerson reads one person (GetUser of the preferences'
// Directory); the audience reads nobody one by one.
func (k *fakeKeycloak) AudiencePerson(_ context.Context, id uuid.UUID) (identity.AudiencePerson, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	p, ok := k.people[id]
	if !ok {
		return identity.AudiencePerson{}, identity.ErrNotFound
	}
	return p, nil
}

// GroupsForUser and GetUser make it the preferences' Directory too.
func (k *fakeKeycloak) GroupsForUser(_ context.Context, id uuid.UUID) ([]identity.Group, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	var out []identity.Group
	for _, g := range k.groups {
		if slices.Contains(k.members[g.ID], id) {
			out = append(out, g)
		}
	}
	return out, nil
}

func (k *fakeKeycloak) GetUser(ctx context.Context, id uuid.UUID) (identity.Person, error) {
	p, err := k.AudiencePerson(ctx, id)
	return p.Person, err
}

// fakeAccounts is core's users table: the people being erased or erased,
// and profile names.
type fakeAccounts struct {
	blocked map[uuid.UUID]bool
	names   map[uuid.UUID][2]string
}

func (a fakeAccounts) Accounts(_ context.Context, ids []uuid.UUID) (map[uuid.UUID]user.Account, error) {
	out := map[uuid.UUID]user.Account{}
	for _, id := range ids {
		if a.blocked[id] {
			out[id] = user.Account{Blocked: true}
		}
		if n, ok := a.names[id]; ok {
			out[id] = user.Account{Stored: true, User: user.User{ID: id, FirstName: n[0], LastName: n[1], AccountState: user.AccountActive}}
		}
	}
	return out, nil
}

// weblabPrivileged is a Privileged root inside WEBLAB: its members are no
// WEBLAB members.
const weblabPrivileged = "/UYELER/ARGE/WEBLAB/YK"

type audienceFixture struct {
	t        *testing.T
	kc       *fakeKeycloak
	accounts fakeAccounts
	store    *MemoryStore
	prefs    *Service
	audience *Audience
	clock    time.Time
	// people by name
	p map[string]uuid.UUID
}

// newAudienceFixture is the test tree with these people (every one
// enabled, verified, with an address, unless said):
//
//	front, back, weblab, leader: in FRONT, BACK, WEBLAB, WEBLAB/LIDERLER
//	weblabYK: only in WEBLAB/YK (a Privileged root inside the team)
//	air, drone: in AIRLAB, AIRLAB/DRONE
//	both: in WEBLAB and AIRLAB
//	follower: in AIRLAB, follows WEBLAB (duyuru)
//	followerFrontOff: in AIRLAB, follows WEBLAB but turned FRONT off
//	outsider: in no /UYELER Group, follows WEBLAB (a row left from before
//	  core refused a non-Member's follow, or a former Member's)
//	projex: in WEBLAB/PROJEX (hidden)
//	gizliFan: in AIRLAB, a choice on the hidden GIZLI
//	disabled, unverified, noEmail, erased, service, dup, teamOff, masterOff: in FRONT
//	clubOff: in AIRLAB, club duyuru off
func newAudienceFixture(t *testing.T, private bool) *audienceFixture {
	t.Helper()
	f := &audienceFixture{
		t:        t,
		kc:       &fakeKeycloak{members: map[string][]uuid.UUID{}, people: map[uuid.UUID]identity.AudiencePerson{}},
		accounts: fakeAccounts{blocked: map[uuid.UUID]bool{}, names: map[uuid.UUID][2]string{}},
		store:    NewMemoryStore(),
		clock:    time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC),
		p:        map[string]uuid.UUID{},
	}
	f.kc.groups = append(testGroups(), identity.Group{ID: "g-weblab-yk", Name: "YK", Path: weblabPrivileged})
	roots := append(authz.DefaultPrivilegedRoots(), weblabPrivileged)
	f.prefs = NewService(TestConfig(testKey(), "https://api.example.test"), f.store, f.kc, roots)
	f.audience = NewAudience(f.prefs, f.kc, f.accounts, private)
	f.audience.SetClock(func() time.Time { return f.clock })

	// Ids grow in the order people are added: front's is below dup's.
	add := func(name string, groups ...string) uuid.UUID {
		id := uuid.MustParse(fmt.Sprintf("00000000-0000-4000-8000-%012d", len(f.p)+1))
		f.p[name] = id
		f.kc.people[id] = identity.AudiencePerson{
			Person:        identity.Person{ID: id, Email: name + "@example.test", FirstName: "KC", LastName: name, Enabled: true},
			EmailVerified: true,
		}
		for _, g := range groups {
			f.kc.members[g] = append(f.kc.members[g], id)
		}
		return id
	}
	add("front", "g-front")
	add("back", "g-back")
	add("weblab", "g-weblab")
	add("leader", "g-weblab-l")
	add("weblabYK", "g-weblab-yk")
	add("air", "g-airlab")
	add("drone", "g-drone")
	add("both", "g-weblab", "g-airlab")
	add("follower", "g-airlab")
	add("followerFrontOff", "g-airlab")
	add("outsider")
	add("projex", "g-projex")
	add("gizliFan", "g-airlab")
	add("clubOff", "g-airlab")
	for _, name := range []string{"disabled", "unverified", "noEmail", "erased", "service", "dup", "teamOff", "masterOff"} {
		add(name, "g-front")
	}
	f.edit("disabled", func(p *identity.AudiencePerson) { p.Enabled = false })
	f.edit("unverified", func(p *identity.AudiencePerson) { p.EmailVerified = false })
	f.edit("noEmail", func(p *identity.AudiencePerson) { p.Email = " " })
	f.edit("service", func(p *identity.AudiencePerson) { p.ServiceAccount = true; p.Username = "service-account-skymail" })
	f.edit("dup", func(p *identity.AudiencePerson) { p.Email = "FRONT@example.test" })
	f.accounts.blocked[f.p["erased"]] = true
	f.accounts.names[f.p["front"]] = [2]string{"Ön", "Uç"}

	f.choose("follower", Team("g-weblab", KindAnnouncement), true)
	f.choose("followerFrontOff", Team("g-weblab", KindAnnouncement), true)
	f.choose("followerFrontOff", Team("g-front", KindAnnouncement), false)
	f.choose("outsider", Team("g-weblab", KindAnnouncement), true)
	f.choose("gizliFan", Team("g-gizli", KindAnnouncement), true)
	f.choose("teamOff", Team("g-front", KindAnnouncement), false)
	f.choose("masterOff", Master(), false)
	f.choose("clubOff", Club(KindAnnouncement), false)
	return f
}

func (f *audienceFixture) edit(name string, change func(*identity.AudiencePerson)) {
	p := f.kc.people[f.p[name]]
	change(&p)
	f.kc.people[f.p[name]] = p
}

func (f *audienceFixture) choose(name string, c Category, on bool) {
	f.t.Helper()
	if err := f.store.Apply(context.Background(), f.p[name], []Change{{Channel: ChannelEmail, Category: c, Enabled: &on}}, SourceAccountCenter, f.clock); err != nil {
		f.t.Fatal(err)
	}
}

func (f *audienceFixture) resolve(req AudienceRequest) AudiencePage {
	f.t.Helper()
	page, err := f.audience.Resolve(context.Background(), req, uuid.Nil, MaxAudiencePage, false)
	if err != nil {
		f.t.Fatalf("resolve %+v: %v", req, err)
	}
	return page
}

// names are the recipients' names in the fixture, sorted.
func (f *audienceFixture) names(items []Recipient) []string {
	byID := map[uuid.UUID]string{}
	for name, id := range f.p {
		byID[id] = name
	}
	var out []string
	for _, r := range items {
		out = append(out, byID[r.UserID])
	}
	slices.Sort(out)
	return out
}

func (f *audienceFixture) item(page AudiencePage, name string) (Recipient, bool) {
	for _, r := range page.Items {
		if r.UserID == f.p[name] {
			return r, true
		}
	}
	return Recipient{}, false
}

func sorted(names ...string) []string {
	out := slices.Clone(names)
	slices.Sort(out)
	return out
}

func duyuruTo(teams ...string) AudienceRequest {
	return AudienceRequest{Type: AudienceTeams, Teams: teams, Kind: KindAnnouncement}
}

// Every Member, once: the WEBLAB/YK member too (in /UYELER), not the
// outsider. The disabled, erased, unverified, address-less and club-off
// people are counted out by reason; the service account is no person and
// not counted; the second account with front's address is counted once.
func TestAudienceMembers(t *testing.T) {
	f := newAudienceFixture(t, false)
	page := f.resolve(AudienceRequest{Type: AudienceMembers, Kind: KindAnnouncement})
	want := sorted("front", "back", "weblab", "leader", "weblabYK", "air", "drone", "both", "follower",
		"followerFrontOff", "projex", "gizliFan", "teamOff")
	if got := f.names(page.Items); !slices.Equal(got, want) {
		t.Fatalf("members = %v, want %v", got, want)
	}
	if page.Counts != (AudienceCounts{Members: len(want), Total: len(want)}) {
		t.Fatalf("counts = %+v", page.Counts)
	}
	// masterOff and clubOff opted out; dup has front's address.
	if page.Skipped != (AudienceSkipped{Disabled: 2, NoEmail: 1, Unverified: 1, OptedOut: 2, DuplicateEmail: 1}) {
		t.Fatalf("skipped = %+v", page.Skipped)
	}
	front, _ := f.item(page, "front")
	if !slices.Equal(front.Categories, []string{"club:duyuru"}) || front.Via != ViaMember || front.Reason != "SKY LAB üyesi olduğun için" ||
		front.CategoryLabel != "Kulüp geneli · Duyurular" || front.Email != "front@example.test" || front.Name != "Ön Uç" {
		t.Fatalf("front = %+v", front)
	}
	if back, _ := f.item(page, "back"); back.Name != "KC back" {
		t.Fatalf("a person core has no row for is named by Keycloak: %+v", back)
	}
	// Every recipient passed the e-mail preference (masterOff and clubOff
	// are left out): the channels SkyMail may use are e-mail (ticket 06).
	for _, r := range page.Items {
		if !slices.Equal(r.AllowedChannels, []Channel{ChannelEmail}) {
			t.Fatalf("%s allowedChannels = %v, want [email]", r.UserID, r.AllowedChannels)
		}
	}
}

// A team's send: its members and its sub-teams' (the hidden PROJEX's
// own members too, and the leaders), not a Privileged subtree inside it;
// and its Member followers. A follower who is no Member is left out and
// counted (Faz 1 mails Members only: SPEC §10.2, §5.8, Karar D7). A choice
// on a sub-team does not touch the team's own mail.
func TestAudienceTeamWithSubTeamsAndFollowers(t *testing.T) {
	f := newAudienceFixture(t, false)
	page := f.resolve(duyuruTo("/UYELER/ARGE/WEBLAB"))
	// teamOff turned FRONT off, which does not reach WEBLAB's own mail.
	members := []string{"front", "back", "weblab", "leader", "both", "projex", "teamOff"}
	followers := []string{"follower", "followerFrontOff"}
	if got := f.names(page.Items); !slices.Equal(got, sorted(append(slices.Clone(members), followers...)...)) {
		t.Fatalf("weblab = %v", got)
	}
	if page.Counts != (AudienceCounts{Members: len(members), Followers: len(followers), Total: len(members) + len(followers)}) {
		t.Fatalf("counts = %+v", page.Counts)
	}
	if page.Skipped != (AudienceSkipped{NotMember: 1, Disabled: 2, NoEmail: 1, Unverified: 1, OptedOut: 1, DuplicateEmail: 1}) {
		t.Fatalf("skipped = %+v", page.Skipped)
	}
	front, _ := f.item(page, "front")
	if !slices.Equal(front.Categories, []string{"team:g-weblab:duyuru"}) || front.Reason != "WEBLAB · Web Geliştirme ekibinde olduğun için" ||
		front.CategoryLabel != "WEBLAB · Web Geliştirme · Duyurular" || front.Via != ViaMember {
		t.Fatalf("front = %+v", front)
	}
	if _, ok := f.item(page, "outsider"); ok {
		t.Fatal("a follower who is no Member is mailed")
	}
	follower, _ := f.item(page, "follower")
	if follower.Via != ViaFollower || follower.Reason != "WEBLAB · Web Geliştirme ekibini takip ettiğin için" {
		t.Fatalf("follower = %+v", follower)
	}
	if _, ok := f.item(page, "weblabYK"); ok {
		t.Fatal("a Privileged subtree inside the team is no team member")
	}
}

// Whoever follows the team above gets the sub-team's mail, unless they
// turned the sub-team off (Karar D5); a member of the team above gets it by
// default (Y1). The person who turned the sub-team off is opted out.
func TestAudienceSubTeamInheritsFromAbove(t *testing.T) {
	f := newAudienceFixture(t, false)
	page := f.resolve(duyuruTo("g-front"))
	want := sorted("front", "back", "weblab", "leader", "both", "projex", "follower")
	if got := f.names(page.Items); !slices.Equal(got, want) {
		t.Fatalf("front = %v, want %v", got, want)
	}
	follower, _ := f.item(page, "follower")
	if follower.Via != ViaFollower || follower.Reason != "WEBLAB · Web Geliştirme ekibini takip ettiğin için" || !slices.Equal(follower.Categories, []string{"team:g-front:duyuru"}) {
		t.Fatalf("follower = %+v", follower)
	}
	back, _ := f.item(page, "back")
	if back.Via != ViaMember || back.Reason != "WEBLAB · Web Geliştirme ekibinde olduğun için" {
		t.Fatalf("back = %+v", back)
	}
	if _, ok := f.item(page, "followerFrontOff"); ok {
		t.Fatal("FRONT turned off, still mailed")
	}
	// followerFrontOff and teamOff turned FRONT off; masterOff everything.
	if page.Skipped.OptedOut != 3 {
		t.Fatalf("skipped = %+v", page.Skipped)
	}
}

// A hidden sub-team's mail reaches its own members only: neither the
// members nor the followers of the team above are candidates, so nobody
// is counted as opted out either. A choice on a hidden team the person is
// not in makes no follower.
func TestAudienceHiddenTeam(t *testing.T) {
	f := newAudienceFixture(t, false)
	page := f.resolve(duyuruTo("/UYELER/ARGE/WEBLAB/PROJEX"))
	if got := f.names(page.Items); !slices.Equal(got, []string{"projex"}) || page.Skipped != (AudienceSkipped{}) {
		t.Fatalf("projex = %v %+v", got, page.Skipped)
	}
	page = f.resolve(duyuruTo("g-gizli"))
	if len(page.Items) != 0 || page.Skipped != (AudienceSkipped{}) {
		t.Fatalf("gizli = %v %+v", f.names(page.Items), page.Skipped)
	}
}

// Someone in two named teams is counted once, with both categories, and
// their unsubscribe link carries both; a membership's reason comes before a
// follow's.
func TestAudienceTwoTeamsOnce(t *testing.T) {
	f := newAudienceFixture(t, false)
	page := f.resolve(duyuruTo("g-weblab", "g-airlab"))
	both, _ := f.item(page, "both")
	if !slices.Equal(both.Categories, []string{"team:g-weblab:duyuru", "team:g-airlab:duyuru"}) || both.CategoryLabel != "WEBLAB · Web Geliştirme, AIRLAB · Duyurular" {
		t.Fatalf("both = %+v", both)
	}
	follower, _ := f.item(page, "follower")
	if follower.Via != ViaMember || follower.Reason != "AIRLAB ekibinde olduğun için" ||
		!slices.Equal(follower.Categories, []string{"team:g-airlab:duyuru", "team:g-weblab:duyuru"}) {
		t.Fatalf("follower = %+v", follower)
	}
	seen := map[uuid.UUID]bool{}
	for _, r := range page.Items {
		if seen[r.UserID] {
			t.Fatalf("%s twice", r.UserID)
		}
		seen[r.UserID] = true
	}
	link, err := f.prefs.ReadLink(context.Background(), linkToken(t, both.UnsubscribeURL))
	if err != nil || len(link.Categories) != 2 || link.Person != f.p["both"] {
		t.Fatalf("link = %+v %v", link, err)
	}
}

// Faz 1 (ADR 0067 karar 3): only public records exist, so only herkes is
// accepted; an absent visibility is herkes.
func TestAudienceFaz1VisibilityIsPublicOnly(t *testing.T) {
	f := newAudienceFixture(t, false)
	for _, req := range []AudienceRequest{
		{Type: AudienceTeams, Teams: []string{"g-weblab"}, Kind: KindAnnouncement, Visibility: VisibilityMembers},
		{Type: AudienceTeams, Teams: []string{"g-weblab"}, Kind: KindAnnouncement, Visibility: VisibilityTeams, VisibilityTeams: []string{"g-front"}},
		{Type: AudienceMembers, Kind: KindNews, Visibility: VisibilityMembers},
	} {
		if _, err := f.audience.Resolve(context.Background(), req, uuid.Nil, 0, true); !errors.Is(err, ErrVisibilityNotEnabled) {
			t.Fatalf("%+v: %v", req, err)
		}
	}
	public := f.resolve(AudienceRequest{Type: AudienceTeams, Teams: []string{"g-weblab"}, Kind: KindAnnouncement, Visibility: VisibilityPublic})
	absent := f.resolve(duyuruTo("g-weblab"))
	if public.Counts != absent.Counts || public.Skipped != absent.Skipped || public.Counts.Followers != 2 || public.Skipped.NotMember != 1 {
		t.Fatalf("public %+v absent %+v", public.Counts, absent.Counts)
	}
}

// The second step: an ekipler record reaches only the people inside its
// visibility teams (with their sub-teams), never a follower; an uyeler
// record no non-Member follower.
func TestAudiencePrivateVisibility(t *testing.T) {
	f := newAudienceFixture(t, true)
	page := f.resolve(AudienceRequest{Type: AudienceTeams, Teams: []string{"g-weblab"}, Kind: KindAnnouncement,
		Visibility: VisibilityTeams, VisibilityTeams: []string{"/UYELER/ARGE/WEBLAB/FRONT"}})
	if got := f.names(page.Items); !slices.Equal(got, sorted("front", "teamOff")) {
		t.Fatalf("ekipler = %v", got)
	}
	// back, weblab, leader, both, projex outside FRONT and the two Member
	// followers; the outsider is no Member; FRONT's disabled, erased,
	// unverified, address-less (and the rest) are counted after visibility.
	if page.Skipped.NotVisible != 7 || page.Skipped.NotMember != 1 || page.Counts.Followers != 0 {
		t.Fatalf("skipped = %+v counts %+v", page.Skipped, page.Counts)
	}
	// Club-wide to the AIRLAB team's eyes only: AIRLAB and DRONE members.
	page = f.resolve(AudienceRequest{Type: AudienceMembers, Kind: KindAnnouncement, Visibility: VisibilityTeams, VisibilityTeams: []string{"g-airlab"}})
	if got := f.names(page.Items); !slices.Equal(got, sorted("air", "drone", "both", "follower", "followerFrontOff", "gizliFan")) {
		t.Fatalf("members/ekipler = %v", got)
	}
	page = f.resolve(AudienceRequest{Type: AudienceTeams, Teams: []string{"g-weblab"}, Kind: KindAnnouncement, Visibility: VisibilityMembers})
	if _, ok := f.item(page, "outsider"); ok || page.Skipped.NotMember != 1 || page.Skipped.NotVisible != 0 || page.Counts.Followers != 2 {
		t.Fatalf("uyeler = %v %+v %+v", f.names(page.Items), page.Skipped, page.Counts)
	}
	if _, err := f.audience.Resolve(context.Background(), AudienceRequest{Type: AudienceMembers, Kind: KindAnnouncement, Visibility: VisibilityTeams}, uuid.Nil, 0, true); !errors.Is(err, ErrInvalid) {
		t.Fatalf("ekipler without teams: %v", err)
	}
	// With private records possible the record's visibility must be said:
	// an absent one could open a private record to the followers (F2).
	for _, req := range []AudienceRequest{duyuruTo("g-weblab"), {Type: AudienceMembers, Kind: KindNews}} {
		if _, err := f.audience.Resolve(context.Background(), req, uuid.Nil, 0, true); !errors.Is(err, ErrInvalid) {
			t.Fatalf("absent visibility with private on: %v", err)
		}
	}
}

// countOnly gives the same counts as the list, and the pages walk every
// recipient once, by user id.
func TestAudienceCountOnlyAndPages(t *testing.T) {
	f := newAudienceFixture(t, false)
	req := duyuruTo("g-weblab", "g-airlab")
	full := f.resolve(req)
	count, err := f.audience.Resolve(context.Background(), req, uuid.Nil, 0, true)
	if err != nil || count.Items != nil || count.Counts != full.Counts || count.Skipped != full.Skipped || count.Counts.Total != len(full.Items) {
		t.Fatalf("count %+v full %+v %v", count, full.Counts, err)
	}
	var walked []Recipient
	after := uuid.Nil
	for pages := 0; ; pages++ {
		page, err := f.audience.Resolve(context.Background(), req, after, 4, false)
		if err != nil || pages > 10 {
			t.Fatalf("page: %v", err)
		}
		if page.Counts != full.Counts || len(page.Items) > 4 {
			t.Fatalf("page counts %+v items %d", page.Counts, len(page.Items))
		}
		walked = append(walked, page.Items...)
		if page.Next == uuid.Nil {
			break
		}
		after = page.Next
	}
	if len(walked) != len(full.Items) {
		t.Fatalf("walked %d of %d", len(walked), len(full.Items))
	}
	for i := range walked {
		if walked[i].UserID != full.Items[i].UserID || (i > 0 && walked[i-1].UserID.String() >= walked[i].UserID.String()) {
			t.Fatalf("page order differs at %d", i)
		}
	}
}

// The link a recipient gets turns off exactly its categories: after its
// POST the person drops out of that send and still gets the others.
func TestAudienceUnsubscribeURLClosesOnlyItsCategories(t *testing.T) {
	f := newAudienceFixture(t, false)
	page := f.resolve(duyuruTo("g-weblab", "g-airlab"))
	both, _ := f.item(page, "both")
	if _, err := f.prefs.Unsubscribe(context.Background(), linkToken(t, both.UnsubscribeURL), ReachCategories); err != nil {
		t.Fatal(err)
	}
	rows, _ := f.store.List(context.Background(), f.p["both"])
	var off []string
	for _, r := range rows {
		if r.Enabled {
			t.Fatalf("row %+v", r)
		}
		off = append(off, r.Category.String())
	}
	slices.Sort(off)
	if !slices.Equal(off, []string{"team:g-airlab:duyuru", "team:g-weblab:duyuru"}) {
		t.Fatalf("off = %v", off)
	}
	f.clock = f.clock.Add(AudienceSnapshotTTL)
	if _, ok := f.item(f.resolve(duyuruTo("g-weblab")), "both"); ok {
		t.Fatal("still in WEBLAB's duyuru")
	}
	for _, req := range []AudienceRequest{
		{Type: AudienceTeams, Teams: []string{"g-weblab"}, Kind: KindNews},
		{Type: AudienceMembers, Kind: KindAnnouncement},
	} {
		if _, ok := f.item(f.resolve(req), "both"); !ok {
			t.Fatalf("%+v lost too", req)
		}
	}
}

func TestAudienceRefusals(t *testing.T) {
	f := newAudienceFixture(t, false)
	ctx := context.Background()
	tooMany := make([]string, MaxAudienceTeams+1)
	for i := range tooMany {
		tooMany[i] = "g-weblab"
	}
	for _, c := range []struct {
		req  AudienceRequest
		want error
	}{
		{AudienceRequest{Type: "everyone", Kind: KindAnnouncement}, ErrInvalid},
		{AudienceRequest{Type: AudienceMembers, Kind: "blog"}, ErrInvalid},
		{AudienceRequest{Type: AudienceMembers, Kind: KindAnnouncement, Teams: []string{"g-weblab"}}, ErrInvalid},
		{AudienceRequest{Type: AudienceTeams, Kind: KindAnnouncement}, ErrInvalid},
		{AudienceRequest{Type: AudienceTeams, Kind: KindAnnouncement, Teams: tooMany}, ErrInvalid},
		{AudienceRequest{Type: AudienceTeams, Kind: KindAnnouncement, Teams: []string{"g-weblab"}, Visibility: "gizli"}, ErrInvalid},
		{AudienceRequest{Type: AudienceTeams, Kind: KindAnnouncement, Teams: []string{"g-weblab"}, VisibilityTeams: []string{"g-front"}}, ErrInvalid},
		{duyuruTo("/UYELER/ARGE/NOPE"), ErrUnknownTeam},
		{duyuruTo("/UYELER/ARGE"), ErrUnknownTeam},
		{duyuruTo("/UYELER/YK"), ErrUnknownTeam},
		{duyuruTo(weblabPrivileged), ErrUnknownTeam},
		{duyuruTo("/UYELER/ARGE/WEBLAB/LIDERLER"), ErrUnknownTeam},
	} {
		if _, err := f.audience.Resolve(ctx, c.req, uuid.Nil, 0, true); !errors.Is(err, c.want) {
			t.Fatalf("%+v: %v, want %v", c.req, err, c.want)
		}
	}
	off := NewAudience(NewService(Config{}, f.store, f.kc, nil), f.kc, f.accounts, false)
	if _, err := off.Resolve(ctx, duyuruTo("g-weblab"), uuid.Nil, 0, true); !errors.Is(err, ErrDisabled) || off.Enabled() {
		t.Fatalf("no key: %v", err)
	}
	f.clock = f.clock.Add(AudienceSnapshotTTL)
	f.kc.fail = errors.New("keycloak down")
	if _, err := f.audience.Resolve(ctx, duyuruTo("g-weblab"), uuid.Nil, 0, true); !errors.Is(err, ErrTeamsUnavailable) {
		t.Fatalf("down: %v", err)
	}
	f.kc.fail = nil
	f.clock = f.clock.Add(AudienceSnapshotTTL)
	f.kc.groups = f.kc.groups[1:] // no /UYELER
	if _, err := f.audience.Resolve(ctx, duyuruTo("g-weblab"), uuid.Nil, 0, true); !errors.Is(err, ErrTeamsUnavailable) {
		t.Fatalf("no members root: %v", err)
	}
}

// One read of Keycloak serves AudienceSnapshotTTL: the live count typed
// against a form and the pages of one send share it.
func TestAudienceSnapshotIsShared(t *testing.T) {
	f := newAudienceFixture(t, false)
	for range 3 {
		f.resolve(duyuruTo("g-weblab"))
	}
	if f.kc.listCalls != 1 {
		t.Fatalf("list calls = %d", f.kc.listCalls)
	}
	f.clock = f.clock.Add(AudienceSnapshotTTL)
	f.resolve(duyuruTo("g-weblab"))
	if f.kc.listCalls != 2 {
		t.Fatalf("list calls after the TTL = %d", f.kc.listCalls)
	}
}

func TestPrivateVisibilityFromEnv(t *testing.T) {
	for raw, want := range map[string]bool{"": false, "off": false, " ON ": true, "on": true} {
		got, err := PrivateVisibilityFromEnv(func(string) string { return raw })
		if err != nil || got != want {
			t.Fatalf("%q: %t %v", raw, got, err)
		}
	}
	if _, err := PrivateVisibilityFromEnv(func(string) string { return "yes" }); err == nil || strings.Contains(err.Error(), "yes") {
		t.Fatalf("bad value: %v", err)
	}
}

// Turning the private visibilities on is said loudly at startup.
func TestAudienceStartupLines(t *testing.T) {
	if lines := AudienceStartupLines(true, false); len(lines) != 1 || lines[0] != "news audience: on (private visibility false)" {
		t.Fatalf("off: %q", lines)
	}
	lines := AudienceStartupLines(true, true)
	if len(lines) != 2 || !strings.HasPrefix(lines[1], "WARNING news audience: "+PrivateVisibilityEnv+"=on") ||
		!strings.Contains(lines[1], "F2") || !strings.Contains(lines[1], "bilet 21") {
		t.Fatalf("on: %q", lines)
	}
}

func linkToken(t *testing.T, link string) string {
	t.Helper()
	u, err := url.Parse(link)
	if err != nil || u.Path != UnsubscribePath {
		t.Fatalf("link %q", link)
	}
	return u.Query().Get("t")
}
