package identity_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/identity"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// teamPageConsents is who objects: false an open team page objection,
// absent (or true) not.
type teamPageConsents struct {
	decided map[uuid.UUID]bool
	err     error
	asked   [][]uuid.UUID
}

func (c *teamPageConsents) TeamPageDecisions(_ context.Context, ids []uuid.UUID) (map[uuid.UUID]bool, error) {
	c.asked = append(c.asked, append([]uuid.UUID(nil), ids...))
	if c.err != nil {
		return nil, c.err
	}
	out := map[uuid.UUID]bool{}
	for _, id := range ids {
		if open, ok := c.decided[id]; ok {
			out[id] = open
		}
	}
	return out, nil
}

type teamPageFixture struct {
	dir      *identity.Memory
	store    *user.MemoryStore
	consents *teamPageConsents
	svc      identity.Service
	member   uuid.UUID
	leader   uuid.UUID
	board    uuid.UUID
}

// newTeamPageFixture is WEBLAB with Ada (member), Grace (leader) and Kemal,
// a WEBLAB member who sits on the Board (/UYELER/YK/BASKAN) and leads it
// (/UYELER/YK/LIDERLER); the Board's Group is listed too. Each has a full
// profile in core.
func newTeamPageFixture(t *testing.T) *teamPageFixture {
	t.Helper()
	f := &teamPageFixture{
		dir: identity.NewMemory(), store: user.NewMemoryStore(), consents: &teamPageConsents{decided: map[uuid.UUID]bool{}},
	}
	f.svc = identity.NewServiceWithOptions(f.dir, f.store, authz.NewAuthorizer(authz.DefaultPolicy()), identity.Options{
		TeamPage: &identity.TeamPageVisibility{Consents: f.consents},
	})
	f.member, f.leader = seedTeam(t, f.dir)
	f.board = uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaa03")
	f.dir.PutGroup(identity.Group{ID: "g-yk", Name: "YK", Path: "/UYELER/YK", Attributes: map[string]string{"public_listing": "true"}})
	f.dir.PutGroup(identity.Group{ID: "g-yk-baskan", Name: "BASKAN", Path: "/UYELER/YK/BASKAN"})
	f.dir.PutGroup(identity.Group{ID: "g-yk-l", Name: "LIDERLER", Path: "/UYELER/YK/LIDERLER"})
	f.dir.PutUser(identity.Person{ID: f.board, Email: "board@example.com", FirstName: "Kemal", LastName: "Board"})
	ctx := context.Background()
	for _, g := range []string{"g-weblab", "g-yk-baskan", "g-yk-l"} {
		if err := f.dir.AddMember(ctx, g, f.board); err != nil {
			t.Fatal(err)
		}
	}
	linkedin := "https://linkedin.com/in/someone"
	for _, id := range []uuid.UUID{f.member, f.leader, f.board} {
		p, err := f.dir.GetUser(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		users := user.NewService(f.store)
		if _, _, err := users.Ensure(ctx, id, user.Profile{Email: p.Email, FirstName: p.FirstName, LastName: p.LastName}); err != nil {
			t.Fatal(err)
		}
		if _, err := users.SetProfilePicture(ctx, id, uuid.New(), "https://cdn.example.test/"+p.FirstName); err != nil {
			t.Fatal(err)
		}
		if _, err := users.Patch(ctx, id, user.ProfilePatch{Linkedin: &linkedin, University: ptrStr("YTÜ"), Faculty: ptrStr("EE"), Department: ptrStr("CE")}); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

func byName(members []identity.PublicMember) map[string]identity.PublicMember {
	out := map[string]identity.PublicMember{}
	for _, m := range members {
		out[m.FirstName] = m
	}
	return out
}

func full(m identity.PublicMember) bool {
	return m.ProfilePictureURL != "" && m.Linkedin != "" && m.University != "" && m.Faculty != "" && m.Department != ""
}

func bare(m identity.PublicMember) bool {
	return m.ProfilePictureURL == "" && m.ProfilePictureSizes == nil && m.Linkedin == "" && m.University == "" &&
		m.Faculty == "" && m.Department == "" && !m.Leader
}

func names(members []identity.PublicMember) []string {
	out := make([]string, 0, len(members))
	for _, m := range members {
		out = append(out, m.FirstName)
	}
	slices.Sort(out)
	return out
}

// A team's public roster shows every member in full unless they object; a
// Board member who objects is not on it either. Objecting hides the person
// at once, lifting the objection shows them again at once.
func TestPublicRosterShowsEveryoneButWhoObjects(t *testing.T) {
	t.Parallel()
	f := newTeamPageFixture(t)
	ctx := context.Background()

	roster, err := f.svc.PublicMembers(ctx, "WEBLAB")
	if err != nil {
		t.Fatal(err)
	}
	got := byName(roster.Members)
	if roster.Count != 3 || !full(got["Ada"]) || !full(got["Grace"]) || !full(got["Kemal"]) {
		t.Fatalf("nobody objects %+v", roster.Members)
	}

	f.consents.decided[f.member] = false
	f.consents.decided[f.board] = false
	roster, err = f.svc.PublicMembers(ctx, "WEBLAB")
	if err != nil || strings.Join(names(roster.Members), ",") != "Grace" || roster.Count != 1 {
		t.Fatalf("after the objections %+v %v", roster.Members, err)
	}

	delete(f.consents.decided, f.member)
	f.consents.decided[f.board] = true
	roster, err = f.svc.PublicMembers(ctx, "WEBLAB")
	if err != nil || roster.Count != 3 || !full(byName(roster.Members)["Ada"]) {
		t.Fatalf("after lifting them %+v %v", roster.Members, err)
	}
}

// The leaders roster follows the same rule; a Board member who leads the
// team and objects is not named as its leader.
func TestPublicLeadersFollowTheTeamPageRule(t *testing.T) {
	t.Parallel()
	f := newTeamPageFixture(t)
	ctx := context.Background()
	if err := f.dir.AddMember(ctx, "g-weblab-l", f.board); err != nil {
		t.Fatal(err)
	}
	leaders, err := f.svc.PublicLeaders(ctx, "WEBLAB")
	if err != nil || leaders.Count != 2 || !full(leaders.Members[0]) || !leaders.Members[0].Leader {
		t.Fatalf("leaders %+v %v", leaders.Members, err)
	}
	f.consents.decided[f.leader] = false
	f.consents.decided[f.board] = false
	leaders, err = f.svc.PublicLeaders(ctx, "WEBLAB")
	if err != nil || leaders.Count != 0 || len(leaders.Members) != 0 {
		t.Fatalf("leaders who object %+v %v", leaders.Members, err)
	}
}

// On the Board's own roster a Board member who objects is named (first and
// last name), nothing more: no picture, school, LinkedIn or leader flag (s6
// row 4 a; Yusuf, 2026-10-27). Without an objection they are shown in full.
func TestBoardRosterNamesBoardMembersWhoObject(t *testing.T) {
	t.Parallel()
	f := newTeamPageFixture(t)
	ctx := context.Background()

	roster, err := f.svc.PublicMembers(ctx, "/UYELER/YK")
	if err != nil || roster.Count != 1 || !full(roster.Members[0]) {
		t.Fatalf("board roster %+v %v", roster.Members, err)
	}
	leaders, err := f.svc.PublicLeaders(ctx, "/UYELER/YK")
	if err != nil || leaders.Count != 1 || !leaders.Members[0].Leader {
		t.Fatalf("board leaders %+v %v", leaders.Members, err)
	}

	f.consents.decided[f.board] = false
	roster, err = f.svc.PublicMembers(ctx, "/UYELER/YK")
	if err != nil {
		t.Fatal(err)
	}
	kemal, ok := byName(roster.Members)["Kemal"]
	if roster.Count != 1 || !ok || kemal.LastName != "Board" || !bare(kemal) {
		t.Fatalf("board roster after the objection %+v", roster.Members)
	}
	leaders, err = f.svc.PublicLeaders(ctx, "/UYELER/YK")
	if err != nil || leaders.Count != 1 || !bare(leaders.Members[0]) {
		t.Fatalf("board leaders after the objection %+v %v", leaders.Members, err)
	}
}

// A roster that cannot read the objections fails: it never falls back to
// showing everyone. Erased people are not even asked about.
func TestPublicRosterFailsWhenTheConsentsCannotBeRead(t *testing.T) {
	t.Parallel()
	f := newTeamPageFixture(t)
	ctx := context.Background()
	if _, err := f.store.RequestDeletion(ctx, f.leader, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.PublicMembers(ctx, "WEBLAB"); err != nil {
		t.Fatal(err)
	}
	if len(f.consents.asked) != 1 || slices.Contains(f.consents.asked[0], f.leader) || len(f.consents.asked[0]) != 2 {
		t.Fatalf("asked %v", f.consents.asked)
	}
	down := errors.New("database unavailable")
	f.consents.err = down
	if _, err := f.svc.PublicMembers(ctx, "WEBLAB"); !errors.Is(err, down) {
		t.Fatalf("members err %v", err)
	}
	if _, err := f.svc.PublicLeaders(ctx, "WEBLAB"); !errors.Is(err, down) {
		t.Fatalf("leaders err %v", err)
	}

	broken := identity.NewServiceWithOptions(f.dir, f.store, authz.NewAuthorizer(authz.DefaultPolicy()), identity.Options{
		TeamPage: &identity.TeamPageVisibility{},
	})
	if _, err := broken.PublicMembers(ctx, "WEBLAB"); err == nil {
		t.Fatal("a rule without a consent source showed a roster")
	}
}

// A listed Group named YK outside the Board's roots is not the Board: a
// member on it who objects is not named.
func TestBoardIsOnlyTheYKRoots(t *testing.T) {
	t.Parallel()
	f := newTeamPageFixture(t)
	f.consents.decided[f.member] = false
	ctx := context.Background()
	f.dir.PutGroup(identity.Group{ID: "g-fake-yk", Name: "SAHTEYK", Path: "/UYELER/ARGE/WEBLAB/YK", Attributes: map[string]string{"public_listing": "true"}})
	if err := f.dir.AddMember(ctx, "g-fake-yk", f.member); err != nil {
		t.Fatal(err)
	}
	roster, err := f.svc.PublicMembers(ctx, "/UYELER/ARGE/WEBLAB/YK")
	if err != nil || roster.Count != 0 {
		t.Fatalf("a WEBLAB subgroup named YK is treated as the Board: %+v %v", roster.Members, err)
	}
}

func TestTeamPageGraceUntilFromEnv(t *testing.T) {
	t.Parallel()
	at, err := identity.TeamPageGraceUntilFromEnv(func(string) string { return "" })
	if err != nil || !at.IsZero() {
		t.Fatalf("unset %v %v", at, err)
	}
	at, err = identity.TeamPageGraceUntilFromEnv(func(string) string { return " 2026-11-10T21:00:00Z " })
	if err != nil || !at.Equal(time.Date(2026, 11, 10, 21, 0, 0, 0, time.UTC)) {
		t.Fatalf("set %v %v", at, err)
	}
	if _, err := identity.TeamPageGraceUntilFromEnv(func(string) string { return "10.11.2026" }); err == nil || !strings.Contains(err.Error(), identity.TeamPageGraceUntilEnv) {
		t.Fatalf("bad %v", err)
	}
}
