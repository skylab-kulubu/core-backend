package commpref

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/identity"
)

type fixture struct {
	t      *testing.T
	dir    *identity.Memory
	store  *MemoryStore
	svc    *Service
	person uuid.UUID
}

// newFixture is a FRONT member (so a WEBLAB member) on the test tree.
func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{t: t, dir: identity.NewMemory(), store: NewMemoryStore(), person: uuid.New()}
	for _, g := range testGroups() {
		f.dir.PutGroup(g)
	}
	f.dir.PutUser(identity.Person{ID: f.person, Email: "uye@example.test"})
	if err := f.dir.AddMember(context.Background(), "g-front", f.person); err != nil {
		t.Fatal(err)
	}
	f.svc = NewService(TestConfig(testKey(), "https://api.example.test"), f.store, f.dir, authz.DefaultPrivilegedRoots())
	f.svc.SetClock(func() time.Time { return time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC) })
	return f
}

func (f *fixture) mine() Screen {
	f.t.Helper()
	s, err := f.svc.Mine(context.Background(), f.person)
	if err != nil {
		f.t.Fatal(err)
	}
	return s
}

func (f *fixture) update(changes ...Change) (Screen, error) {
	return f.svc.Update(context.Background(), f.person, changes)
}

func set(c Category, v bool) Change { return Change{Channel: ChannelEmail, Category: c, Enabled: &v} }
func forget(c Category) Change      { return Change{Channel: ChannelEmail, Category: c} }

func ids(rows []TeamRow) []string {
	var out []string
	for _, r := range rows {
		out = append(out, r.GroupID)
		out = append(out, ids(r.Children)...)
	}
	return out
}

func find(rows []TeamRow, id string) (TeamRow, bool) {
	for _, r := range rows {
		if r.GroupID == id {
			return r, true
		}
		if found, ok := find(r.Children, id); ok {
			return found, true
		}
	}
	return TeamRow{}, false
}

// A Member's screen by default: master and club on, their team tree with
// every sub-team, all on (a sub-team they are not in from the team above,
// Y1), the public teams outside it to follow, and no hidden or Privileged
// team anywhere. Every switch is given per channel.
func TestScreenDefaults(t *testing.T) {
	f := newFixture(t)
	s := f.mine()
	if !s.Master[ChannelEmail].Effective || s.Master[ChannelEmail].Explicit != nil || !s.Club.Duyuru[ChannelEmail].Effective || !s.Club.Haber[ChannelEmail].Effective {
		t.Fatalf("master/club = %+v %+v", s.Master, s.Club)
	}
	if got := ids(s.Teams); !slices.Equal(got, []string{"g-weblab", "g-back", "g-front"}) {
		t.Fatalf("teams = %v", got)
	}
	weblab, _ := find(s.Teams, "g-weblab")
	front, _ := find(s.Teams, "g-front")
	back, _ := find(s.Teams, "g-back")
	if !weblab.Member || !weblab.Kinds.Duyuru[ChannelEmail].Effective || weblab.DisplayName != "Web Geliştirme" ||
		!front.Member || !front.Kinds.Haber[ChannelEmail].Effective ||
		back.Member || !back.Kinds.Duyuru[ChannelEmail].Effective || back.Kinds.Duyuru[ChannelEmail].Origin != OriginDefault ||
		back.Kinds.Duyuru[ChannelEmail].From != "g-weblab" || len(back.Kinds.Duyuru) != 1 || len(s.Master) != 1 ||
		len(s.Channels) != 1 || s.Channels[0] != ChannelEmail {
		t.Fatalf("weblab=%+v front=%+v back=%+v", weblab, front, back)
	}
	if len(s.Following) != 0 {
		t.Fatalf("following = %+v", s.Following)
	}
	var followable []string
	for _, r := range s.Followable {
		followable = append(followable, r.GroupID)
	}
	if !slices.Equal(followable, []string{"g-airlab"}) {
		t.Fatalf("followable = %v", followable)
	}
}

// Following a public team: it moves from followable to following with its
// sub-teams, which inherit (D5); a sub-team turned off stays off; the
// choice is forgotten with null and the team is followable again.
func TestFollowAndInheritance(t *testing.T) {
	f := newFixture(t)
	s, err := f.update(set(Team("g-airlab", KindAnnouncement), true), set(Team("g-weblab", KindNews), true), set(Team("g-front", KindNews), false))
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(s.Following); !slices.Equal(got, []string{"g-airlab", "g-drone"}) || len(s.Followable) != 0 {
		t.Fatalf("following=%v followable=%v", got, s.Followable)
	}
	drone, _ := find(s.Following, "g-drone")
	if !drone.Kinds.Duyuru[ChannelEmail].Effective || drone.Kinds.Duyuru[ChannelEmail].Origin != OriginInherited || drone.Kinds.Duyuru[ChannelEmail].From != "g-airlab" || drone.Kinds.Haber[ChannelEmail].Effective {
		t.Fatalf("drone = %+v", drone.Kinds)
	}
	back, _ := find(s.Teams, "g-back")
	front, _ := find(s.Teams, "g-front")
	if !back.Kinds.Haber[ChannelEmail].Effective || back.Kinds.Haber[ChannelEmail].From != "g-weblab" || front.Kinds.Haber[ChannelEmail].Effective || front.Kinds.Haber[ChannelEmail].Explicit == nil || *front.Kinds.Haber[ChannelEmail].Explicit {
		t.Fatalf("back=%+v front=%+v", back.Kinds, front.Kinds)
	}
	s, err = f.update(forget(Team("g-airlab", KindAnnouncement)))
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Following) != 0 || len(s.Followable) != 1 {
		t.Fatalf("after unfollow following=%v followable=%v", ids(s.Following), s.Followable)
	}
	rows, _ := f.store.List(context.Background(), f.person)
	for _, row := range rows {
		if row.Source != SourceAccountCenter {
			t.Fatalf("row %+v", row)
		}
	}
}

// Only a team the person may see can be turned on or off (D7, O2): a hidden
// team they are not in (also a hidden sub-team of their own team), a
// Privileged Group, an area and an unknown id all answer the same, so the
// answer tells nothing of a hidden team (D1). Forgetting a choice is always
// allowed.
func TestUpdateRefusesHiddenAndUnknownTeams(t *testing.T) {
	f := newFixture(t)
	for _, id := range []string{"g-gizli", "g-projex", "g-yk", "g-yk-alt", "g-arge", "g-uyeler", "g-weblab-l", "nope"} {
		for _, v := range []bool{true, false} {
			if _, err := f.update(set(Team(id, KindAnnouncement), v)); !errors.Is(err, ErrUnknownTeam) {
				t.Fatalf("%s %v: %v", id, v, err)
			}
		}
	}
	if _, err := f.update(set(Team("g-back", KindNews), false)); err != nil {
		t.Fatalf("off for a listed sub-team of their own team: %v", err)
	}
	if _, err := f.update(forget(Team("deleted-team", KindNews)), forget(Team("g-gizli", KindNews))); err != nil {
		t.Fatalf("forget a deleted or hidden team: %v", err)
	}
	for name, changes := range map[string][]Change{
		"none":      nil,
		"twice":     {set(Club(KindNews), false), set(Club(KindNews), true)},
		"malformed": {set(Category{Scope: ScopeClub}, false)},
	} {
		if _, err := f.update(changes...); !errors.Is(err, ErrInvalid) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	rows, _ := f.store.List(context.Background(), f.person)
	if len(rows) != 1 || rows[0].Category != Team("g-back", KindNews) {
		t.Fatalf("rows = %+v", rows)
	}
}

// O2 and D1 on the screen: a hidden sub-team of the person's own team is
// not shown to a non-member; a choice left on a hidden team (an old link,
// a former membership) shows masked in following, names nothing, and can
// be forgotten. A member of the hidden team sees it, on by default.
func TestScreenHidesHiddenTeams(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if err := f.store.Apply(ctx, f.person, []Change{set(Team("g-gizli", KindNews), false), set(Team("g-projex", KindNews), false)}, SourceUnsubscribeLink, time.Now()); err != nil {
		t.Fatal(err)
	}
	s := f.mine()
	if _, ok := find(s.Teams, "g-projex"); ok {
		t.Fatalf("hidden sub-team shown: %v", ids(s.Teams))
	}
	if len(s.Following) != 2 {
		t.Fatalf("following = %+v", s.Following)
	}
	for _, r := range s.Following {
		if !r.Hidden || r.Code != "" || r.Path != "" || r.DisplayName != HiddenTeamName || len(r.Children) != 0 || r.Kinds.Haber[ChannelEmail].Effective {
			t.Fatalf("not masked: %+v", r)
		}
	}
	for _, r := range s.Followable {
		if r.GroupID == "g-gizli" || r.GroupID == "g-projex" {
			t.Fatalf("hidden team followable: %+v", s.Followable)
		}
	}
	s, err := f.update(forget(Team("g-gizli", KindNews)), forget(Team("g-projex", KindNews)))
	if err != nil || len(s.Following) != 0 {
		t.Fatalf("after forgetting: %+v %v", s.Following, err)
	}

	if err := f.dir.AddMember(ctx, "g-projex", f.person); err != nil {
		t.Fatal(err)
	}
	projex, ok := find(f.mine().Teams, "g-projex")
	if !ok || !projex.Member || projex.Hidden || projex.Code != "PROJEX" || !projex.Kinds.Duyuru[ChannelEmail].Effective {
		t.Fatalf("member's own hidden team: %+v %v", projex, ok)
	}
}

// countingDirectory counts the tree reads.
type countingDirectory struct {
	*identity.Memory
	lists int
}

func (d *countingDirectory) ListGroups(ctx context.Context) ([]identity.Group, error) {
	d.lists++
	return d.Memory.ListGroups(ctx)
}

// O1: the screen, its changes and the public page share one tree read a
// minute.
func TestTreeIsReadOnceAMinute(t *testing.T) {
	f := newFixture(t)
	dir := &countingDirectory{Memory: f.dir}
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	svc := NewService(f.svc.Config(), f.store, dir, authz.DefaultPrivilegedRoots())
	svc.SetClock(func() time.Time { return now })
	ctx := context.Background()
	for range 3 {
		if _, err := svc.Mine(ctx, f.person); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := svc.Update(ctx, f.person, []Change{set(Club(KindNews), false)}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ReadLink(ctx, token(t, svc, f.person, Club(KindNews))); err != nil {
		t.Fatal(err)
	}
	if dir.lists != 1 {
		t.Fatalf("tree read %d times", dir.lists)
	}
	now = now.Add(TreeTTL)
	if _, err := svc.Mine(ctx, f.person); err != nil || dir.lists != 2 {
		t.Fatalf("after a minute: %d reads, %v", dir.lists, err)
	}
}

// D2: the rows of a person Keycloak no longer has go; a Keycloak error, or
// too many people gone at once, removes nothing.
func TestRemoveDeletedPeople(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	gone := uuid.New()
	for _, person := range []uuid.UUID{f.person, gone} {
		if err := f.store.Apply(ctx, person, []Change{set(Club(KindNews), false)}, SourceAccountCenter, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	broken := NewService(f.svc.Config(), f.store, failingDirectory{}, nil)
	if n, err := broken.RemoveDeletedPeople(ctx); err == nil || n != 0 {
		t.Fatalf("keycloak down: %d %v", n, err)
	}
	if n, err := f.svc.RemoveDeletedPeople(ctx); err != nil || n != 1 {
		t.Fatalf("removed %d %v", n, err)
	}
	if rows := must(f.store.List(ctx, f.person)); len(rows) != 1 {
		t.Fatalf("a known person lost rows: %v", rows)
	}
	for range 6 {
		if err := f.store.Apply(ctx, uuid.New(), []Change{set(Club(KindNews), false)}, SourceAccountCenter, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := f.svc.RemoveDeletedPeople(ctx); !errors.Is(err, errTooManyGone) || n != 0 {
		t.Fatalf("too many gone: %d %v", n, err)
	}
	if subjects := must(f.store.Subjects(ctx)); len(subjects) != 7 {
		t.Fatalf("subjects = %d", len(subjects))
	}
}

// The master switch off: everything is off and every row stays; on again,
// the earlier settings are back.
func TestMasterSwitchKeepsTheSettings(t *testing.T) {
	f := newFixture(t)
	if _, err := f.update(set(Club(KindAnnouncement), false), set(Team("g-airlab", KindNews), true)); err != nil {
		t.Fatal(err)
	}
	s, err := f.update(set(Master(), false))
	if err != nil {
		t.Fatal(err)
	}
	airlab, _ := find(s.Following, "g-airlab")
	weblab, _ := find(s.Teams, "g-weblab")
	if s.Master[ChannelEmail].Effective || s.Club.Haber[ChannelEmail].Effective || !s.Club.Haber[ChannelEmail].Enabled || s.Club.Duyuru[ChannelEmail].Enabled ||
		airlab.Kinds.Haber[ChannelEmail].Effective || !airlab.Kinds.Haber[ChannelEmail].Enabled || weblab.Kinds.Duyuru[ChannelEmail].Effective || !weblab.Kinds.Duyuru[ChannelEmail].Enabled {
		t.Fatalf("master off: %+v %+v %+v %+v", s.Master, s.Club, airlab.Kinds, weblab.Kinds)
	}
	s, err = f.update(set(Master(), true))
	if err != nil {
		t.Fatal(err)
	}
	airlab, _ = find(s.Following, "g-airlab")
	if !s.Club.Haber[ChannelEmail].Effective || s.Club.Duyuru[ChannelEmail].Effective || !airlab.Kinds.Haber[ChannelEmail].Effective {
		t.Fatalf("master on: %+v %+v", s.Club, airlab.Kinds)
	}
}

func token(t *testing.T, svc *Service, person uuid.UUID, cats ...Category) string {
	t.Helper()
	link, err := svc.UnsubscribeURL(person, ChannelEmail, cats)
	if err != nil {
		t.Fatal(err)
	}
	parsed, _ := url.Parse(link)
	return parsed.Query().Get("t")
}

// The link: reading it changes nothing; the POST turns off exactly its
// categories, and a second one leaves the same rows; the page's wider
// choices turn off the sources, or everything.
func TestUnsubscribeLink(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	tok := token(t, f.svc, f.person, Team("g-weblab", KindAnnouncement), Team("g-airlab", KindAnnouncement))
	link, err := f.svc.ReadLink(ctx, tok)
	if err != nil || link.Person != f.person || len(link.Categories) != 2 ||
		link.Categories[0].Label != "AIRLAB · Duyurular" || link.Categories[1].Label != "WEBLAB · Duyurular" {
		t.Fatalf("link = %+v %v", link, err)
	}
	if rows, _ := f.store.List(ctx, f.person); len(rows) != 0 {
		t.Fatalf("reading the link wrote %+v", rows)
	}
	for range 2 {
		if _, err := f.svc.Unsubscribe(ctx, tok, ReachCategories); err != nil {
			t.Fatal(err)
		}
		e := ExplicitOf(must(f.store.List(ctx, f.person)), ChannelEmail)
		if len(e) != 2 || e[Team("g-weblab", KindAnnouncement)] || e[Team("g-airlab", KindAnnouncement)] {
			t.Fatalf("explicit = %v", e)
		}
	}
	s := f.mine()
	weblab, _ := find(s.Teams, "g-weblab")
	back, _ := find(s.Teams, "g-back")
	if weblab.Kinds.Duyuru[ChannelEmail].Effective || !weblab.Kinds.Haber[ChannelEmail].Effective || back.Kinds.Duyuru[ChannelEmail].Effective || !s.Club.Duyuru[ChannelEmail].Effective {
		t.Fatalf("after one click: %+v %+v %+v", weblab.Kinds, back.Kinds, s.Club)
	}
	rows := must(f.store.List(ctx, f.person))
	if rows[0].Source != SourceUnsubscribeLink {
		t.Fatalf("source = %s", rows[0].Source)
	}

	if _, err := f.svc.Unsubscribe(ctx, tok, ReachSources); err != nil {
		t.Fatal(err)
	}
	e := ExplicitOf(must(f.store.List(ctx, f.person)), ChannelEmail)
	if len(e) != 4 || e[Team("g-weblab", KindNews)] || e[Team("g-airlab", KindNews)] {
		t.Fatalf("sources = %v", e)
	}
	if _, err := f.svc.Unsubscribe(ctx, tok, ReachAll); err != nil {
		t.Fatal(err)
	}
	if s := f.mine(); s.Master[ChannelEmail].Effective || s.Club.Duyuru[ChannelEmail].Effective {
		t.Fatalf("all: %+v", s.Master)
	}

	forged := []byte(tok)
	forged[len(forged)-1] ^= 1
	if _, err := f.svc.Unsubscribe(ctx, string(forged), ReachCategories); !errors.Is(err, ErrLink) {
		t.Fatalf("forged: %v", err)
	}
	if _, err := f.svc.ReadLink(ctx, string(forged)); !errors.Is(err, ErrLink) {
		t.Fatalf("forged read: %v", err)
	}
}

// Keycloak not answering, or a deleted team, still shows the page with a
// general name.
func TestReadLinkWithoutTeamNames(t *testing.T) {
	f := newFixture(t)
	svc := NewService(f.svc.Config(), f.store, failingDirectory{}, nil)
	link, err := svc.ReadLink(context.Background(), token(t, f.svc, f.person, Team("g-weblab", KindNews), Club(KindAnnouncement)))
	if err != nil || link.Categories[0].Label != "Kulüp geneli · Duyurular" || link.Categories[1].Label != "Bir ekip · Haberler" {
		t.Fatalf("link = %+v %v", link, err)
	}
}

type failingDirectory struct{}

func (failingDirectory) ListGroups(context.Context) ([]identity.Group, error) {
	return nil, errors.New("keycloak down")
}
func (failingDirectory) GroupsForUser(context.Context, uuid.UUID) ([]identity.Group, error) {
	return nil, errors.New("keycloak down")
}
func (failingDirectory) GetUser(context.Context, uuid.UUID) (identity.Person, error) {
	return identity.Person{}, errors.New("keycloak down")
}

func TestKeycloakDownIsTeamsUnavailable(t *testing.T) {
	f := newFixture(t)
	svc := NewService(f.svc.Config(), f.store, failingDirectory{}, nil)
	if _, err := svc.Mine(context.Background(), f.person); !errors.Is(err, ErrTeamsUnavailable) {
		t.Fatalf("mine: %v", err)
	}
	if _, err := svc.Update(context.Background(), f.person, []Change{set(Club(KindNews), false)}); !errors.Is(err, ErrTeamsUnavailable) {
		t.Fatalf("update: %v", err)
	}
}

// Without the key every route is off; erasure still deletes.
func TestDisabledServiceRefusesButErases(t *testing.T) {
	f := newFixture(t)
	tok := token(t, f.svc, f.person, Club(KindNews))
	if _, err := f.update(set(Club(KindNews), false)); err != nil {
		t.Fatal(err)
	}
	off := NewService(Config{}, f.store, f.dir, nil)
	ctx := context.Background()
	if _, err := off.Mine(ctx, f.person); !errors.Is(err, ErrDisabled) {
		t.Fatal(err)
	}
	if _, err := off.Update(ctx, f.person, []Change{set(Club(KindNews), true)}); !errors.Is(err, ErrDisabled) {
		t.Fatal(err)
	}
	if _, err := off.ReadLink(ctx, tok); !errors.Is(err, ErrDisabled) {
		t.Fatal(err)
	}
	if _, err := off.Unsubscribe(ctx, tok, ReachCategories); !errors.Is(err, ErrDisabled) {
		t.Fatal(err)
	}
	if _, err := off.UnsubscribeURL(f.person, ChannelEmail, []Category{Club(KindNews)}); !errors.Is(err, ErrDisabled) {
		t.Fatal(err)
	}
	if n, err := off.EraseSubject(ctx, f.person); err != nil || n != 1 {
		t.Fatalf("erase: %d %v", n, err)
	}
	if n, err := off.EraseSubject(ctx, f.person); err != nil || n != 0 {
		t.Fatalf("erase again: %d %v", n, err)
	}
}

func TestUnsubscribeForAnInactivePersonWritesNothing(t *testing.T) {
	f := newFixture(t)
	f.store.MarkInactive(f.person)
	if _, err := f.svc.Unsubscribe(context.Background(), token(t, f.svc, f.person, Club(KindNews)), ReachCategories); !errors.Is(err, ErrSubjectInactive) {
		t.Fatalf("err = %v", err)
	}
}

// A deleted team's rows go; a Group list without /UYELER deletes nothing.
func TestRemoveDeletedTeams(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	other := uuid.New()
	if err := f.store.Apply(ctx, other, []Change{set(Team("g-airlab", KindNews), true), set(Team("g-gone", KindNews), false), set(Club(KindNews), false)}, SourceAccountCenter, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := f.store.Apply(ctx, f.person, []Change{set(Team("g-gone", KindAnnouncement), true)}, SourceAccountCenter, time.Now()); err != nil {
		t.Fatal(err)
	}
	broken := NewService(f.svc.Config(), f.store, rootless{}, nil)
	if n, err := broken.RemoveDeletedTeams(ctx); err == nil || n != 0 {
		t.Fatalf("rootless: %d %v", n, err)
	}
	if n, err := f.svc.RemoveDeletedTeams(ctx); err != nil || n != 2 {
		t.Fatalf("removed %d %v", n, err)
	}
	if e := ExplicitOf(must(f.store.List(ctx, other)), ChannelEmail); len(e) != 2 || !e[Team("g-airlab", KindNews)] {
		t.Fatalf("other = %v", e)
	}
	if rows := must(f.store.List(ctx, f.person)); len(rows) != 0 {
		t.Fatalf("person = %v", rows)
	}
}

type rootless struct{}

func (rootless) ListGroups(context.Context) ([]identity.Group, error) {
	return []identity.Group{{ID: "g-x", Name: "X", Path: "/X"}}, nil
}
func (rootless) GroupsForUser(context.Context, uuid.UUID) ([]identity.Group, error) { return nil, nil }
func (rootless) GetUser(context.Context, uuid.UUID) (identity.Person, error) {
	return identity.Person{}, nil
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

// countedGroups counts the Group list reads (one per run of the deleted
// teams part) and when each came.
type countedGroups struct {
	Directory
	mu    sync.Mutex
	reads []time.Time
}

func (c *countedGroups) ListGroups(ctx context.Context) ([]identity.Group, error) {
	c.mu.Lock()
	c.reads = append(c.reads, time.Now())
	c.mu.Unlock()
	return c.Directory.ListGroups(ctx)
}

func (c *countedGroups) runs() []time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.reads)
}

func waitRuns(t *testing.T, c *countedGroups, n int) []time.Time {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if runs := c.runs(); len(runs) >= n {
			return runs
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("%d runs, want %d", len(c.runs()), n)
	return nil
}

// A wake runs the deleted teams part soon, outside the interval: wakes that
// come before it runs make one run, and a run comes no sooner than the gap
// after the last.
func TestWakeTeamCleanup(t *testing.T) {
	f := newFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	other := uuid.New()
	gone := func() {
		t.Helper()
		if err := f.store.Apply(ctx, other, []Change{set(Team("g-gone", KindNews), true)}, SourceAccountCenter, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	gone()
	groups := &countedGroups{Directory: f.dir}
	svc := NewService(f.svc.Config(), f.store, groups, authz.DefaultPrivilegedRoots())
	const gap = 150 * time.Millisecond
	svc.SetTeamCleanupGap(gap)
	var mu sync.Mutex
	var reports []string
	done := MaintainCleanup(ctx, svc, time.Hour, func(what string, removed int64, err error) {
		mu.Lock()
		defer mu.Unlock()
		reports = append(reports, fmt.Sprintf("%s %d %v", what, removed, err))
	})
	defer func() {
		cancel()
		<-done
	}()
	first := waitRuns(t, groups, 1)[0]

	// A Group is deleted in Keycloak: two events, one run, the gap after
	// the first.
	gone()
	svc.WakeTeamCleanup()
	svc.WakeTeamCleanup()
	second := waitRuns(t, groups, 2)[1]
	if second.Sub(first) < gap {
		t.Fatalf("second run %v after the first, want at least %v", second.Sub(first), gap)
	}
	time.Sleep(3 * gap)
	if runs := groups.runs(); len(runs) != 2 {
		t.Fatalf("%d runs, want 2 (the wakes make one)", len(runs))
	}
	if rows := must(f.store.List(ctx, other)); len(rows) != 0 {
		t.Fatalf("the deleted team's rows stay: %v", rows)
	}
	mu.Lock()
	if !slices.Equal(reports, []string{"deleted teams 1 <nil>", "deleted teams 1 <nil>"}) {
		t.Fatalf("reports = %v", reports)
	}
	mu.Unlock()
}

// A wake never blocks: without a running cleanup, or on a nil service.
func TestWakeTeamCleanupNeverBlocks(t *testing.T) {
	f := newFixture(t)
	for range 3 {
		f.svc.WakeTeamCleanup()
	}
	var none *Service
	none.WakeTeamCleanup()
}
