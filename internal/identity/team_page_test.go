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

// teamPageConsents is the people holding an open team page consent.
type teamPageConsents struct {
	ids   map[uuid.UUID]bool
	err   error
	asked [][]uuid.UUID
}

func (c *teamPageConsents) TeamPageAccounts(_ context.Context, ids []uuid.UUID) (map[uuid.UUID]bool, error) {
	c.asked = append(c.asked, append([]uuid.UUID(nil), ids...))
	if c.err != nil {
		return nil, c.err
	}
	out := map[uuid.UUID]bool{}
	for _, id := range ids {
		if c.ids[id] {
			out[id] = true
		}
	}
	return out, nil
}

type teamPageFixture struct {
	dir      *identity.Memory
	store    *user.MemoryStore
	consents *teamPageConsents
	now      time.Time
	svc      identity.Service
	member   uuid.UUID
	leader   uuid.UUID
	board    uuid.UUID
}

// newTeamPageFixture is WEBLAB with Ada (member), Grace (leader) and Kemal,
// a WEBLAB member who sits on the Board (/UYELER/YK/BASKAN), each with a
// full profile in core.
func newTeamPageFixture(t *testing.T, graceUntil time.Time) *teamPageFixture {
	t.Helper()
	f := &teamPageFixture{
		dir: identity.NewMemory(), store: user.NewMemoryStore(), consents: &teamPageConsents{ids: map[uuid.UUID]bool{}},
		now: time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC),
	}
	f.svc = identity.NewServiceWithOptions(f.dir, f.store, authz.NewAuthorizer(authz.DefaultPolicy()), identity.Options{
		TeamPage: &identity.TeamPageVisibility{Consents: f.consents, GraceUntil: graceUntil, Now: func() time.Time { return f.now }},
	})
	f.member, f.leader = seedTeam(t, f.dir)
	f.board = uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaa03")
	f.dir.PutGroup(identity.Group{ID: "g-yk-baskan", Name: "BASKAN", Path: "/UYELER/YK/BASKAN"})
	f.dir.PutUser(identity.Person{ID: f.board, Email: "board@example.com", FirstName: "Kemal", LastName: "Board"})
	ctx := context.Background()
	for _, g := range []string{"g-weblab", "g-yk-baskan"} {
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
	return m.ProfilePictureURL == "" && m.ProfilePictureSizes == nil && m.Linkedin == "" && m.University == "" && m.Faculty == "" && m.Department == ""
}

// After the transition a public roster shows only who consented, in full;
// a Board member without consent by name (and leader flag) only; nobody
// else. Consenting shows the person at once, withdrawing hides them at once.
func TestPublicRosterShowsOnlyConsentingMembersAndTheBoardsNames(t *testing.T) {
	t.Parallel()
	f := newTeamPageFixture(t, time.Time{})
	ctx := context.Background()

	roster, err := f.svc.PublicMembers(ctx, "WEBLAB")
	if err != nil {
		t.Fatal(err)
	}
	got := byName(roster.Members)
	if roster.Count != 1 || len(got) != 1 {
		t.Fatalf("nobody consented: %+v", roster.Members)
	}
	if kemal, ok := got["Kemal"]; !ok || kemal.LastName != "Board" || !bare(kemal) {
		t.Fatalf("board member without consent %+v", got["Kemal"])
	}

	f.consents.ids[f.member] = true
	f.consents.ids[f.board] = true
	roster, err = f.svc.PublicMembers(ctx, "WEBLAB")
	if err != nil {
		t.Fatal(err)
	}
	got = byName(roster.Members)
	if roster.Count != 2 || !full(got["Ada"]) || !full(got["Kemal"]) {
		t.Fatalf("after consent %+v", roster.Members)
	}
	if _, ok := got["Grace"]; ok {
		t.Fatal("the leader without consent is listed")
	}

	delete(f.consents.ids, f.member)
	roster, err = f.svc.PublicMembers(ctx, "WEBLAB")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := byName(roster.Members)["Ada"]; ok || roster.Count != 1 {
		t.Fatalf("after withdrawal %+v", roster.Members)
	}
}

// The leaders roster follows the same rule.
func TestPublicLeadersFollowTheTeamPageRule(t *testing.T) {
	t.Parallel()
	f := newTeamPageFixture(t, time.Time{})
	ctx := context.Background()
	leaders, err := f.svc.PublicLeaders(ctx, "WEBLAB")
	if err != nil || leaders.Count != 0 || len(leaders.Members) != 0 {
		t.Fatalf("leaders without consent %+v %v", leaders.Members, err)
	}
	f.consents.ids[f.leader] = true
	leaders, err = f.svc.PublicLeaders(ctx, "WEBLAB")
	if err != nil || leaders.Count != 1 || !leaders.Members[0].Leader || !full(leaders.Members[0]) {
		t.Fatalf("leaders %+v %v", leaders.Members, err)
	}

	// A Board member who leads the team is named as its leader without
	// consent, nothing more.
	if err := f.dir.AddMember(ctx, "g-weblab-l", f.board); err != nil {
		t.Fatal(err)
	}
	leaders, err = f.svc.PublicLeaders(ctx, "WEBLAB")
	if err != nil {
		t.Fatal(err)
	}
	kemal := byName(leaders.Members)["Kemal"]
	if leaders.Count != 2 || !kemal.Leader || !bare(kemal) {
		t.Fatalf("board leader %+v", leaders.Members)
	}
}

// Until the transition ends the rosters stay as they were: everyone, in
// full, with no consent read.
func TestPublicRosterIsUnchangedUntilTheTransitionEnds(t *testing.T) {
	t.Parallel()
	graceUntil := time.Date(2026, 11, 10, 21, 0, 0, 0, time.UTC)
	f := newTeamPageFixture(t, graceUntil)
	ctx := context.Background()
	roster, err := f.svc.PublicMembers(ctx, "WEBLAB")
	if err != nil {
		t.Fatal(err)
	}
	got := byName(roster.Members)
	if roster.Count != 3 || !full(got["Ada"]) || !full(got["Grace"]) || !full(got["Kemal"]) {
		t.Fatalf("during the transition %+v", roster.Members)
	}
	if len(f.consents.asked) != 0 {
		t.Fatal("consents read during the transition")
	}

	f.now = graceUntil
	roster, err = f.svc.PublicMembers(ctx, "WEBLAB")
	if err != nil || roster.Count != 1 {
		t.Fatalf("at the end of the transition %+v %v", roster.Members, err)
	}
}

// A roster that cannot read the consents fails: it never falls back to
// showing everyone. Erased people are not even asked about.
func TestPublicRosterFailsWhenTheConsentsCannotBeRead(t *testing.T) {
	t.Parallel()
	f := newTeamPageFixture(t, time.Time{})
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

// A Group named YK outside the Board's roots is not the Board.
func TestBoardIsOnlyTheYKRoots(t *testing.T) {
	t.Parallel()
	f := newTeamPageFixture(t, time.Time{})
	ctx := context.Background()
	f.dir.PutGroup(identity.Group{ID: "g-fake-yk", Name: "YK", Path: "/UYELER/ARGE/WEBLAB/YK"})
	if err := f.dir.AddMember(ctx, "g-fake-yk", f.member); err != nil {
		t.Fatal(err)
	}
	roster, err := f.svc.PublicMembers(ctx, "WEBLAB")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := byName(roster.Members)["Ada"]; ok {
		t.Fatalf("a member of a WEBLAB subgroup named YK is shown as the Board: %+v", roster.Members)
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
