package identity_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
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

// The members' seat search (ekip-yetkileri ticket 04): a member without
// users:read finds Forms' seat holders who are members, at most ten, by
// name, e-mail masked; and every seat search finds a holder through a
// Group beyond Keycloak's first fifty, by core's profile name, Turkish
// letters folded.

type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

type memberRealm struct {
	dir   *identity.Memory
	store *user.MemoryStore
	svc   identity.Service
	clock *clock
	ids   map[string]uuid.UUID
}

var formsSeat = identity.ClientRole{ClientID: "forms", Role: "skyforms:access"}

// newMemberRealm is a realm where the top Team WEBLAB maps the seat, so its
// Sub-team FRONT and its Leader group inherit it, and /ROLLER/FORMS-ACCESS
// gives it on purpose. options may switch things off.
func newMemberRealm(t *testing.T, options identity.Options) memberRealm {
	t.Helper()
	dir := identity.NewMemory()
	store := user.NewMemoryStore()
	c := &clock{now: time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)}
	options.Now = c.Now
	svc := identity.NewServiceWithOptions(dir, store, authz.NewAuthorizer(authz.DefaultPolicy()), options)
	r := memberRealm{dir: dir, store: store, svc: svc, clock: c, ids: map[string]uuid.UUID{}}
	for _, g := range []identity.Group{
		{ID: "g-uyeler", Name: "UYELER", Path: "/UYELER"},
		{ID: "g-arge", Name: "ARGE", Path: "/UYELER/ARGE"},
		{ID: "g-weblab", Name: "WEBLAB", Path: "/UYELER/ARGE/WEBLAB"},
		{ID: "g-weblab-leaders", Name: "LIDERLER", Path: "/UYELER/ARGE/WEBLAB/LIDERLER"},
		{ID: "g-front", Name: "FRONT", Path: "/UYELER/ARGE/WEBLAB/FRONT"},
		{ID: "g-algolab", Name: "ALGOLAB", Path: "/UYELER/ARGE/ALGOLAB"},
		{ID: "g-roller", Name: "ROLLER", Path: "/ROLLER"},
		{ID: "g-forms-access", Name: "FORMS-ACCESS", Path: "/ROLLER/FORMS-ACCESS"},
	} {
		dir.PutGroup(g)
	}
	r.must(t, dir.SetGroupClientRoles(context.Background(), "g-weblab", []identity.ClientRole{formsSeat}))
	r.must(t, dir.SetGroupClientRoles(context.Background(), "g-forms-access", []identity.ClientRole{formsSeat}))
	return r
}

func (r memberRealm) must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// person puts a person named first last into the Groups.
func (r memberRealm) person(t *testing.T, who, first, last string, groups ...string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	r.ids[who] = id
	r.dir.PutUser(identity.Person{ID: id, Email: who + "@example.test", FirstName: first, LastName: last})
	for _, g := range groups {
		r.must(t, r.dir.AddMember(context.Background(), g, id))
	}
	return id
}

func (r memberRealm) names(t *testing.T, people []identity.Person) []string {
	t.Helper()
	out := make([]string, 0, len(people))
	for _, p := range people {
		found := false
		for who, id := range r.ids {
			if id == p.ID {
				out = append(out, who)
				found = true
			}
		}
		if !found {
			t.Fatalf("unknown person in the answer: %+v", p)
		}
	}
	slices.Sort(out)
	return out
}

func (r memberRealm) ops(name string) int {
	n := 0
	for _, op := range r.dir.Ops {
		if op == name {
			n++
		}
	}
	return n
}

// aMember is a person in a Sub-team, with no core role in the token.
func aMember() authz.Principal {
	return authz.Principal{ID: uuid.NewString(), Groups: []string{"/UYELER/ARGE/WEBLAB/FRONT"}, Client: "skyforms"}
}

func usersReader() authz.Principal {
	return authz.Principal{ID: uuid.NewString(), Groups: []string{"/UYELER/ARGE/WEBLAB"}, Roles: []string{"users:read"}}
}

func TestMemberSeatSearchFindsMembersWithTheSeatMaskedAndAtMostTen(t *testing.T) {
	t.Parallel()
	r := newMemberRealm(t, identity.Options{})
	for i := range 12 {
		group := "g-weblab"
		if i%2 == 1 {
			group = "g-front"
		}
		r.person(t, fmt.Sprintf("holder%02d", i), "Ayşe", fmt.Sprintf("Kaya%02d", i), group)
	}
	r.person(t, "signedup", "Ayşe", "Kendi", "g-uyeler")  // no seat, /UYELER only
	r.person(t, "otherteam", "Ayşe", "Algo", "g-algolab") // a member, no seat
	got, err := r.svc.ListUsers(context.Background(), aMember(), "ayse", formsSeat)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 10 {
		t.Fatalf("answer has %d people, want 10: %v", len(got), r.names(t, got))
	}
	for i, p := range got {
		if want := fmt.Sprintf("Kaya%02d", i); p.LastName != want {
			t.Errorf("answer %d is %s, want %s (sorted by last name)", i, p.LastName, want)
		}
		if p.Email != "h***@example.test" {
			t.Errorf("e-mail %q, want it masked", p.Email)
		}
		if p.Username != "" || p.SchoolEmail != "" || p.SkyNumber != "" || p.Status != user.ReadStatusActive {
			t.Errorf("answer carries more than the safe card: %+v", p)
		}
	}
	// users:read reads as before: every match, unmasked.
	all, err := r.svc.ListUsers(context.Background(), usersReader(), "ayse", formsSeat)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 12 || !strings.HasPrefix(all[0].Email, "holder") {
		t.Fatalf("users:read answer = %v", r.names(t, all))
	}
}

func TestMemberSeatSearchShortQueryAnswersNobodyWithoutReading(t *testing.T) {
	t.Parallel()
	r := newMemberRealm(t, identity.Options{})
	r.person(t, "holder", "Ada", "Lovelace", "g-weblab")
	for _, q := range []string{"", "a", "ad", " ad "} {
		got, err := r.svc.ListUsers(context.Background(), aMember(), q, formsSeat)
		if err != nil || got == nil || len(got) != 0 {
			t.Fatalf("q %q: %v %v, want an empty list", q, got, err)
		}
	}
	if n := r.ops("ClientRoleHolders") + r.ops("SearchUsers"); n != 0 {
		t.Fatalf("a short query read the directory %d times", n)
	}
	got, err := r.svc.ListUsers(context.Background(), aMember(), "ada", formsSeat)
	if err != nil || len(got) != 1 {
		t.Fatalf("three letters: %v %v", got, err)
	}
}

func TestMemberSeatSearchRefusesWhoIsNoMember(t *testing.T) {
	t.Parallel()
	r := newMemberRealm(t, identity.Options{})
	r.person(t, "holder", "Ada", "Lovelace", "g-weblab")
	for name, p := range map[string]authz.Principal{
		"signed up, /UYELER only": {ID: uuid.NewString(), Groups: []string{"/UYELER"}},
		"an area only":            {ID: uuid.NewString(), Groups: []string{"/UYELER/ARGE"}},
		"a role group only":       {ID: uuid.NewString(), Groups: []string{"/ROLLER/FORMS-ACCESS"}},
		"no groups claim":         {ID: uuid.NewString()},
		"outside the tree":        {ID: uuid.NewString(), Groups: []string{"/KONSOL"}},
		"a service account":       {ID: uuid.NewString(), Groups: []string{"/UYELER/ARGE/WEBLAB"}, ServiceAccount: true},
		"a product":               {ID: uuid.NewString(), Groups: []string{"/UYELER/ARGE/WEBLAB"}, Product: authz.ProductForms},
	} {
		if got, err := r.svc.ListUsers(context.Background(), p, "ada", formsSeat); !errors.Is(err, identity.ErrForbidden) {
			t.Errorf("%s: %v %v, want ErrForbidden", name, got, err)
		}
	}
	// A member asks only for Forms' seat, and never the whole list.
	for _, seat := range [][]identity.ClientRole{
		{{ClientID: "forms", Role: "skyforms:*"}},
		{{ClientID: "forms", Role: "skyforms:read-all"}},
		{{ClientID: "skymail", Role: "skyforms:access"}},
		nil,
	} {
		if got, err := r.svc.ListUsers(context.Background(), aMember(), "ada", seat...); !errors.Is(err, identity.ErrForbidden) {
			t.Errorf("seat %v: %v %v, want ErrForbidden", seat, got, err)
		}
	}
	// A Privileged root is a member, a YK duty group too.
	for _, path := range []string{"/UYELER/YK/SEKRETER", "/UYELER/ARGE/WEBLAB/LIDERLER", "/UYELER/ARGE/ALGOLAB"} {
		p := authz.Principal{ID: uuid.NewString(), Groups: []string{path}}
		if got, err := r.svc.ListUsers(context.Background(), p, "ada", formsSeat); err != nil || len(got) != 1 {
			t.Errorf("%s: %v %v, want the holder", path, got, err)
		}
	}
}

// A person whose token names /UYELER and /ROLLER/FORMS-ACCESS only is no
// member, but holds the seat in effect: they search as a member does
// (ekip-yetkileri ticket 16). Without the seat, or as a service account or
// a product, they are refused.
func TestMemberSeatSearchSeatHolderSearchesAsAMember(t *testing.T) {
	t.Parallel()
	r := newMemberRealm(t, identity.Options{})
	for i := range 12 {
		r.person(t, fmt.Sprintf("holder%02d", i), "Ayşe", fmt.Sprintf("Kaya%02d", i), "g-weblab")
	}
	groups := []string{"/UYELER", "/ROLLER/FORMS-ACCESS"}
	seated := r.person(t, "seated", "Zeynep", "Koltuk", "g-uyeler", "g-forms-access")
	caller := authz.Principal{ID: seated.String(), Groups: groups, Client: "skyforms"}
	got, err := r.svc.ListUsers(context.Background(), caller, "ayse", formsSeat)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 10 {
		t.Fatalf("answer has %d people, want 10: %v", len(got), r.names(t, got))
	}
	for _, p := range got {
		if p.Email != "h***@example.test" {
			t.Errorf("e-mail %q, want it masked", p.Email)
		}
	}
	if got, err := r.svc.ListUsers(context.Background(), caller, "ay", formsSeat); err != nil || len(got) != 0 {
		t.Fatalf("two letters: %v %v, want an empty list", got, err)
	}
	// The seat is read once a minute.
	reads := r.ops("EffectiveClientRoles")
	if _, err := r.svc.ListUsers(context.Background(), caller, "ayse", formsSeat); err != nil {
		t.Fatal(err)
	}
	if n := r.ops("EffectiveClientRoles"); n != reads {
		t.Fatalf("the caller's seat read again within the minute: %d, then %d", reads, n)
	}

	// The same token, the seat gone: refused.
	unseated := r.person(t, "unseated", "Zeynep", "Koltuksuz", "g-uyeler")
	for name, p := range map[string]authz.Principal{
		"no seat":           {ID: unseated.String(), Groups: groups, Client: "skyforms"},
		"a service account": {ID: seated.String(), Groups: groups, ServiceAccount: true},
		"a product":         {ID: seated.String(), Groups: groups, Product: authz.ProductForms},
		"not a uuid":        {ID: "seated", Groups: groups},
	} {
		if got, err := r.svc.ListUsers(context.Background(), p, "ayse", formsSeat); !errors.Is(err, identity.ErrForbidden) {
			t.Errorf("%s: %v %v, want ErrForbidden", name, got, err)
		}
	}
	// Only Forms' seat, and the switch still turns it off.
	if got, err := r.svc.ListUsers(context.Background(), caller, "ayse", identity.ClientRole{ClientID: "forms", Role: "skyforms:read-all"}); !errors.Is(err, identity.ErrForbidden) {
		t.Errorf("another seat: %v %v, want ErrForbidden", got, err)
	}
	off := newMemberRealm(t, identity.Options{NoMemberSeatSearch: true})
	offSeated := off.person(t, "seated", "Zeynep", "Koltuk", "g-uyeler", "g-forms-access")
	if got, err := off.svc.ListUsers(context.Background(), authz.Principal{ID: offSeated.String(), Groups: groups}, "ayse", formsSeat); !errors.Is(err, identity.ErrForbidden) {
		t.Errorf("switched off: %v %v, want ErrForbidden", got, err)
	}
}

func TestMemberSeatSearchSwitchedOffAnswersForbidden(t *testing.T) {
	t.Parallel()
	r := newMemberRealm(t, identity.Options{NoMemberSeatSearch: true})
	r.person(t, "holder", "Ada", "Lovelace", "g-weblab")
	if got, err := r.svc.ListUsers(context.Background(), aMember(), "ada", formsSeat); !errors.Is(err, identity.ErrForbidden) {
		t.Fatalf("switched off: %v %v, want ErrForbidden", got, err)
	}
	if got, err := r.svc.ListUsers(context.Background(), usersReader(), "ada", formsSeat); err != nil || len(got) != 1 {
		t.Fatalf("users:read with the switch off: %v %v", got, err)
	}
}

func TestMemberSeatSearchWholeAddressIsUnmaskedAndAPartFindsNobody(t *testing.T) {
	t.Parallel()
	r := newMemberRealm(t, identity.Options{})
	r.person(t, "ada.l", "Ada", "Lovelace", "g-weblab")
	r.person(t, "ada.b", "Ada", "Byron", "g-weblab")
	got, err := r.svc.ListUsers(context.Background(), aMember(), "ADA.L@example.test", formsSeat)
	if err != nil || len(got) != 1 || got[0].Email != "ada.l@example.test" {
		t.Fatalf("whole address: %+v %v", got, err)
	}
	for _, q := range []string{"ada.l@", "@example.test", "ada.l@example", "example.test", "ada.l"} {
		got, err := r.svc.ListUsers(context.Background(), aMember(), q, formsSeat)
		if err != nil || len(got) != 0 {
			t.Errorf("part of an address %q: %v %v, want nobody", q, r.names(t, got), err)
		}
	}
}

func TestMemberSeatSearchFindsOnlyMembers(t *testing.T) {
	t.Parallel()
	r := newMemberRealm(t, identity.Options{})
	ctx := context.Background()
	r.person(t, "team", "Seat", "Team", "g-weblab")
	r.person(t, "leader", "Seat", "Leader", "g-weblab-leaders")
	r.person(t, "rolegroup", "Seat", "Rolegroup", "g-forms-access")
	// The seat on the person, a member of another Team: a member.
	direct := r.person(t, "direct", "Seat", "Direct", "g-algolab")
	r.must(t, r.dir.AddUserExtraRole(ctx, direct, formsSeat))
	// The seat on the person, in /UYELER only: not a member.
	lone := r.person(t, "directlone", "Seat", "Lone", "g-uyeler")
	r.must(t, r.dir.AddUserExtraRole(ctx, lone, formsSeat))
	// The seat through the default roles: a member of a Team is found, a
	// signed-up account in /UYELER is not.
	defaults := identity.ClientRole{Role: "default-roles-e-skylab"}
	r.dir.PutCompositeRole(defaults, formsSeat)
	r.dir.SetDefaultRoles(defaults)
	r.person(t, "defaultmember", "Seat", "Algo", "g-algolab")
	r.person(t, "defaultsignedup", "Seat", "Signedup", "g-uyeler")
	got, err := r.svc.ListUsers(ctx, aMember(), "seat", formsSeat)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"defaultmember", "direct", "leader", "rolegroup", "team"}
	if names := r.names(t, got); !slices.Equal(names, want) {
		t.Fatalf("members' answer = %v, want %v", names, want)
	}
	all, err := r.svc.ListUsers(ctx, usersReader(), "seat", formsSeat)
	if err != nil {
		t.Fatal(err)
	}
	want = []string{"defaultmember", "defaultsignedup", "direct", "directlone", "leader", "rolegroup", "team"}
	if names := r.names(t, all); !slices.Equal(names, want) {
		t.Fatalf("users:read answer = %v, want %v", names, want)
	}
}

func TestMemberSeatSearchIsRateLimited(t *testing.T) {
	t.Parallel()
	r := newMemberRealm(t, identity.Options{})
	r.person(t, "holder", "Ada", "Lovelace", "g-weblab")
	caller := aMember()
	for i := range 30 {
		if _, err := r.svc.ListUsers(context.Background(), caller, "ada", formsSeat); err != nil {
			t.Fatalf("search %d: %v", i+1, err)
		}
	}
	_, err := r.svc.ListUsers(context.Background(), caller, "ada", formsSeat)
	var refused *identity.DelegationError
	if !errors.As(err, &refused) || refused.Status != http.StatusTooManyRequests || refused.Code != identity.CodeRateLimited || refused.RetryAfter != 60 {
		t.Fatalf("31st search: %v, want 429 rate_limited", err)
	}
	// Another member has a budget of their own, and a users:read caller none.
	if _, err := r.svc.ListUsers(context.Background(), aMember(), "ada", formsSeat); err != nil {
		t.Fatalf("another member: %v", err)
	}
	for range 40 {
		if _, err := r.svc.ListUsers(context.Background(), usersReader(), "ada", formsSeat); err != nil {
			t.Fatalf("users:read: %v", err)
		}
	}
	r.clock.add(61 * time.Second)
	if _, err := r.svc.ListUsers(context.Background(), caller, "ada", formsSeat); err != nil {
		t.Fatalf("after a minute: %v", err)
	}
}

func TestSeatSearchKeepsTheHoldersAMinute(t *testing.T) {
	t.Parallel()
	r := newMemberRealm(t, identity.Options{})
	ctx := context.Background()
	r.person(t, "holder", "Ada", "Lovelace", "g-front")
	search := func() []string {
		t.Helper()
		got, err := r.svc.ListUsers(ctx, aMember(), "ada", formsSeat)
		if err != nil {
			t.Fatal(err)
		}
		return r.names(t, got)
	}
	search()
	search()
	if n := r.ops("ClientRoleHolders"); n != 1 {
		t.Fatalf("holders read %d times in a minute, want 1", n)
	}
	// Added through the console (straight to the directory): within the
	// minute found through Keycloak's search and their effective roles, the
	// holders not read again.
	late := r.person(t, "late", "Ada", "Byron", "g-front")
	if names := search(); !slices.Equal(names, []string{"holder", "late"}) {
		t.Fatalf("within the minute: %v", names)
	}
	if n := r.ops("ClientRoleHolders"); n != 1 {
		t.Fatalf("holders read %d times in a minute, want 1", n)
	}
	reads := r.ops("EffectiveClientRoles")
	search()
	if n := r.ops("EffectiveClientRoles"); n != reads {
		t.Fatalf("effective roles read again within the minute: %d, then %d", reads, n)
	}
	r.clock.add(seatTTL)
	if names := search(); !slices.Equal(names, []string{"holder", "late"}) {
		t.Fatalf("after the minute: %v", names)
	}
	if n := r.ops("ClientRoleHolders"); n != 2 {
		t.Fatalf("holders read %d times, want 2", n)
	}
	// Removed through core: what was kept is dropped at once.
	if err := r.svc.RemoveMember(ctx, privileged(), "g-front", late); err != nil {
		t.Fatal(err)
	}
	if names := search(); !slices.Equal(names, []string{"holder"}) {
		t.Fatalf("after a removal through core: %v", names)
	}
	if n := r.ops("ClientRoleHolders"); n != 3 {
		t.Fatalf("holders read %d times, want 3", n)
	}
}

const seatTTL = time.Minute

// More than fifty people match a short query in Keycloak's search, the
// seat holder among them past the fiftieth: found, as the holders through
// a Group are read whole.
func TestSeatSearchFindsAGroupHolderPastKeycloaksFirstFifty(t *testing.T) {
	t.Parallel()
	r := newMemberRealm(t, identity.Options{})
	for i := range 60 {
		r.person(t, fmt.Sprintf("ali%02d", i), "Ali", fmt.Sprintf("Aaa%02d", i), "g-algolab")
	}
	r.person(t, "holder", "Ali", "Zzz", "g-front")
	for name, p := range map[string]authz.Principal{"member": aMember(), "users:read": usersReader(), "privileged": privileged()} {
		got, err := r.svc.ListUsers(context.Background(), p, "ali", formsSeat)
		if err != nil {
			t.Fatal(err)
		}
		if names := r.names(t, got); !slices.Equal(names, []string{"holder"}) {
			t.Errorf("%s: %v, want the holder", name, names)
		}
	}
}

func TestSeatSearchFoldsTurkishLetters(t *testing.T) {
	t.Parallel()
	r := newMemberRealm(t, identity.Options{})
	r.person(t, "isik", "Işık", "Yılmaz", "g-front")
	r.person(t, "ascii", "Isik", "Celik", "g-weblab")
	for q, want := range map[string][]string{
		"isik":   {"ascii", "isik"},
		"IŞIK":   {"ascii", "isik"},
		"ışık":   {"ascii", "isik"},
		"İşık":   {"ascii", "isik"},
		"yilmaz": {"isik"},
		"çelik":  {"ascii"},
		"ŞIK Y":  {"isik"},
	} {
		for name, p := range map[string]authz.Principal{"member": aMember(), "users:read": usersReader()} {
			got, err := r.svc.ListUsers(context.Background(), p, q, formsSeat)
			if err != nil {
				t.Fatal(err)
			}
			if names := r.names(t, got); !slices.Equal(names, want) {
				t.Errorf("%s %q: %v, want %v", name, q, names, want)
			}
		}
	}
}

// A name changed only in core's profile is found, for a caller who reads
// the safe card as for a member.
func TestSeatSearchFindsANameChangedInCoreOnly(t *testing.T) {
	t.Parallel()
	r := newMemberRealm(t, identity.Options{})
	id := r.person(t, "renamed", "Eski", "Ad", "g-front")
	if _, _, err := r.store.Upsert(context.Background(), user.User{ID: id, Email: "renamed@example.test", FirstName: "Yeni", LastName: "Soyad"}); err != nil {
		t.Fatal(err)
	}
	for name, p := range map[string]authz.Principal{"member": aMember(), "users:read": usersReader(), "privileged": privileged()} {
		got, err := r.svc.ListUsers(context.Background(), p, "yeni soy", formsSeat)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0].ID != id || got[0].FirstName != "Yeni" {
			t.Errorf("%s: %+v, want the person by core's name", name, got)
		}
	}
}

func TestParseMemberSeatSearch(t *testing.T) {
	t.Parallel()
	for raw, want := range map[string]bool{"": true, "on": true, " on ": true, "off": false} {
		got, err := identity.ParseMemberSeatSearch(raw)
		if err != nil || got != want {
			t.Errorf("%q: %v %v, want %v", raw, got, err, want)
		}
	}
	for _, raw := range []string{"true", "0", "ON"} {
		if _, err := identity.ParseMemberSeatSearch(raw); err == nil || !strings.Contains(err.Error(), identity.MemberSeatSearchEnv) {
			t.Errorf("%q: %v, want an error naming the variable", raw, err)
		}
	}
}

// groupKeycloak is a Keycloak whose seat is mapped to the top Team WEBLAB
// alone; its Sub-team FRONT inherits it. Keycloak's search finds nobody
// for a query without the Turkish letters, as Keycloak does not fold them.
type groupKeycloak struct {
	mu    sync.Mutex
	calls map[string]int
}

const (
	kcWeblab = "11111111-1111-1111-1111-111111111111"
	kcFront  = "22222222-2222-2222-2222-222222222222"
)

var (
	kcLeader = uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	kcIsik   = uuid.MustParse("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")
)

func (f *groupKeycloak) count(what string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[what]
}

func (f *groupKeycloak) directory(t *testing.T) *identity.Keycloak {
	t.Helper()
	f.calls = map[string]int{}
	groups := map[string]map[string]any{
		kcWeblab: {"id": kcWeblab, "name": "WEBLAB", "path": "/UYELER/ARGE/WEBLAB"},
		kcFront:  {"id": kcFront, "name": "FRONT", "path": "/UYELER/ARGE/WEBLAB/FRONT"},
	}
	members := map[string][]map[string]any{
		kcWeblab: {{"id": kcLeader.String(), "username": "leader", "email": "leader@example.test", "firstName": "Işıl", "lastName": "Lider", "enabled": true}},
		kcFront:  {{"id": kcIsik.String(), "username": "isik", "email": "isik@example.test", "firstName": "Işık", "lastName": "Yılmaz", "enabled": true}},
	}
	children := map[string][]map[string]any{kcWeblab: {groups[kcFront]}, kcFront: {}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/protocol/openid-connect/token") {
			_, _ = w.Write([]byte(`{"access_token":"tok","expires_in":300,"token_type":"Bearer"}`))
			return
		}
		const realm = "/admin/realms/e-skylab"
		path := r.URL.EscapedPath()
		hit := func(what string) {
			f.mu.Lock()
			f.calls[what]++
			f.mu.Unlock()
		}
		switch {
		case path == realm+"/clients" && r.URL.Query().Get("clientId") == "forms":
			_, _ = w.Write([]byte(`[{"id":"c-forms","clientId":"forms"}]`))
		case path == realm+"/clients/c-forms/roles/skyforms:access/groups":
			hit("role groups")
			_ = json.NewEncoder(w).Encode([]map[string]any{groups[kcWeblab]})
		case strings.HasPrefix(path, realm+"/clients/c-forms/roles/"):
			_, _ = w.Write([]byte(`[]`))
		case strings.HasPrefix(path, realm+"/groups/"):
			rest := strings.TrimPrefix(path, realm+"/groups/")
			id, tail, _ := strings.Cut(rest, "/")
			g, ok := groups[id]
			if !ok {
				http.NotFound(w, r)
				return
			}
			switch tail {
			case "":
				_ = json.NewEncoder(w).Encode(g)
			case "members":
				hit("members")
				_ = json.NewEncoder(w).Encode(members[id])
			case "children":
				_ = json.NewEncoder(w).Encode(children[id])
			default:
				t.Errorf("unexpected Keycloak call %s %s", r.Method, path)
				http.NotFound(w, r)
			}
		case path == realm+"/users" && r.URL.Query().Has("search"):
			hit("search")
			_, _ = w.Write([]byte(`[]`))
		case strings.HasPrefix(path, realm+"/users/") && strings.HasSuffix(path, "/groups"):
			hit("user groups")
			_, _ = w.Write([]byte(`[]`))
		default:
			t.Errorf("unexpected Keycloak call %s %s", r.Method, path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return identity.NewKeycloak(identity.KeycloakConfig{URL: srv.URL, Realm: "e-skylab", ClientID: "core", ClientSecret: "secret"})
}

// Against Keycloak itself: the Sub-team's member, who holds the seat
// through the top Team above, is found by a query Keycloak's search does
// not fold ("isik" for Işık), and the second search reads no Group.
func TestKeycloakSeatSearchWalksTheGroupsUnderTheMappedOne(t *testing.T) {
	t.Parallel()
	fake := &groupKeycloak{}
	svc := identity.NewService(fake.directory(t), user.NewMemoryStore(), authz.NewAuthorizer(authz.DefaultPolicy()))
	for range 2 {
		got, err := svc.ListUsers(context.Background(), aMember(), "isik", formsSeat)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0].ID != kcIsik || got[0].Email != "i***@example.test" {
			t.Fatalf("members' search = %+v, want Işık masked", got)
		}
	}
	got, err := svc.ListUsers(context.Background(), usersReader(), "isi", formsSeat)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("users:read search = %+v, want Işıl and Işık", got)
	}
	if n := fake.count("role groups"); n != 1 {
		t.Fatalf("role groups read %d times, want 1 (kept a minute)", n)
	}
	if n := fake.count("members"); n != 2 {
		t.Fatalf("members read %d times, want 2 (WEBLAB and FRONT, once)", n)
	}
	if n := fake.count("user groups"); n != 0 {
		t.Fatalf("a holder through a Team had their Groups read %d times", n)
	}
}
