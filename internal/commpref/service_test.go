package commpref

import (
	"context"
	"errors"
	"net/url"
	"slices"
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

// Only a visible team may be followed (D7); a hidden team, a Privileged
// Group, an area or an unknown id cannot. Turning one off, or forgetting a
// choice for a deleted team, is allowed.
func TestUpdateRefusesHiddenAndUnknownTeams(t *testing.T) {
	f := newFixture(t)
	for _, id := range []string{"g-gizli", "g-yk", "g-yk-alt", "g-arge", "g-uyeler", "g-weblab-l", "nope"} {
		if _, err := f.update(set(Team(id, KindAnnouncement), true)); !errors.Is(err, ErrUnknownTeam) {
			t.Fatalf("%s: %v", id, err)
		}
	}
	if _, err := f.update(set(Team("nope", KindAnnouncement), false)); !errors.Is(err, ErrUnknownTeam) {
		t.Fatalf("off for an unknown team: %v", err)
	}
	if _, err := f.update(set(Team("g-gizli", KindNews), false)); err != nil {
		t.Fatalf("off for a hidden team: %v", err)
	}
	if _, err := f.update(forget(Team("deleted-team", KindNews))); err != nil {
		t.Fatalf("forget a deleted team: %v", err)
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
	if len(rows) != 1 || rows[0].Category != Team("g-gizli", KindNews) {
		t.Fatalf("rows = %+v", rows)
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

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}
