package dashboard_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/dashboard"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/identity"
)

// now is a Saturday afternoon in Istanbul (UTC+3).
var now = time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)

func at(t time.Time) *time.Time { return &t }

// fakeStore answers fixed aggregates and records which Events it was asked
// about.
type fakeStore struct {
	mu      sync.Mutex
	counts  map[uuid.UUID]dashboard.TicketCounts
	daily   map[uuid.UUID]map[string]int
	blocked map[uuid.UUID]bool
	asked   [][]uuid.UUID
	since   time.Time
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		counts:  map[uuid.UUID]dashboard.TicketCounts{},
		daily:   map[uuid.UUID]map[string]int{},
		blocked: map[uuid.UUID]bool{},
	}
}

func (s *fakeStore) TicketCounts(_ context.Context, ids []uuid.UUID) (map[uuid.UUID]dashboard.TicketCounts, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.asked = append(s.asked, slices.Clone(ids))
	out := map[uuid.UUID]dashboard.TicketCounts{}
	for _, id := range ids {
		if c, ok := s.counts[id]; ok {
			out[id] = c
		}
	}
	return out, nil
}

func (s *fakeStore) DailyApplications(_ context.Context, ids []uuid.UUID, since time.Time, _ *time.Location) (map[uuid.UUID]map[string]int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.asked = append(s.asked, slices.Clone(ids))
	s.since = since
	out := map[uuid.UUID]map[string]int{}
	for _, id := range ids {
		if d, ok := s.daily[id]; ok {
			out[id] = d
		}
	}
	return out, nil
}

func (s *fakeStore) BlockedAccounts(_ context.Context, ids []uuid.UUID) (map[uuid.UUID]bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[uuid.UUID]bool{}
	for _, id := range ids {
		if s.blocked[id] {
			out[id] = true
		}
	}
	return out, nil
}

type fixture struct {
	events *event.MemoryStore
	store  *fakeStore
	dir    *identity.Memory
	clock  time.Time
	svc    dashboard.Service

	weblabSoon, weblabLive, weblabOld, gecekoduSoon, noTeam, archived event.Event
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{events: event.NewMemoryStore(), store: newFakeStore(), dir: identity.NewMemory(), clock: now}
	create := func(e event.Event) event.Event {
		created, err := f.events.Create(t.Context(), e)
		if err != nil {
			t.Fatal(err)
		}
		return created
	}
	f.weblabSoon = create(event.Event{Name: "WebLab Bootcamp", OwnerTeam: "WEBLAB", Location: "D-101", Capacity: 50,
		StartDate: at(now.Add(5 * 24 * time.Hour)), EndDate: at(now.Add(5*24*time.Hour + 3*time.Hour)), Active: true})
	// No end date: running for half a day from its start.
	f.weblabLive = create(event.Event{Name: "WebLab Meetup", OwnerTeam: "WEBLAB", Location: "D-102",
		StartDate: at(now.Add(-2 * time.Hour)), FormURL: "https://skyl.app/meetup", CoverImageURL: "events/cover.webp"})
	f.weblabOld = create(event.Event{Name: "WebLab Spring", OwnerTeam: "WEBLAB",
		StartDate: at(now.AddDate(0, -3, 0)), EndDate: at(now.AddDate(0, -3, 0).Add(time.Hour))})
	f.gecekoduSoon = create(event.Event{Name: "Gece Kodu", OwnerTeam: "GECEKODU",
		StartDate: at(now.Add(10 * 24 * time.Hour)), ExtraFormURLs: []event.EventFormLink{{Label: "Başvuru", URL: "https://skyl.app/gk"}}})
	f.noTeam = create(event.Event{Name: "Genel Kurul", StartDate: at(now.Add(-40 * 24 * time.Hour))})
	f.archived = create(event.Event{Name: "Arşiv", OwnerTeam: "WEBLAB", StartDate: at(now.Add(24 * time.Hour))})
	if err := f.events.Archive(t.Context(), f.archived.ID, nil); err != nil {
		t.Fatal(err)
	}
	f.store.counts[f.weblabSoon.ID] = dashboard.TicketCounts{Applications: 46, Members: 30, Guests: 16}
	f.store.counts[f.weblabLive.ID] = dashboard.TicketCounts{Applications: 10, Members: 4, Guests: 6, CheckedIn: 7}
	f.store.counts[f.gecekoduSoon.ID] = dashboard.TicketCounts{Applications: 3, Members: 3}
	f.store.counts[f.archived.ID] = dashboard.TicketCounts{Applications: 99}
	f.store.daily[f.weblabSoon.ID] = map[string]int{"2026-10-03": 2, "2026-10-01": 1, "2026-09-04": 5, "2026-09-03": 100}
	f.store.daily[f.gecekoduSoon.ID] = map[string]int{"2026-10-03": 1}
	f.svc = f.service(t, dashboard.DefaultMembersTTL)
	return f
}

func (f *fixture) service(t *testing.T, ttl time.Duration) dashboard.Service {
	t.Helper()
	svc, err := dashboard.NewService(dashboard.Options{
		Events: f.events, Store: f.store, Authz: authz.NewAuthorizer(authz.DefaultPolicy()),
		Directory: f.dir, MembersTTL: ttl, Now: func() time.Time { return f.clock },
	})
	if err != nil {
		t.Fatal(err)
	}
	return svc
}

func eventNames(stats []dashboard.EventStat) []string {
	out := make([]string, len(stats))
	for i, s := range stats {
		out[i] = s.Name
	}
	return out
}

var (
	privileged = authz.Principal{ID: uuid.NewString(), Groups: []string{"/UYELER/YK"}}
	leader     = authz.Principal{ID: uuid.NewString(), Groups: []string{"/UYELER/ARGE/WEBLAB/LIDERLER"}}
)

// A team leader's summary covers their own team's Events and nothing of
// another team's, by the rule core already applies to an Event's applicant
// list; the store is never asked about the rest.
func TestSummaryCoversWhatTheCallerMayReadTheApplicationsOf(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	got, err := f.svc.Summary(t.Context(), leader)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got.OwnerTeams, []string{"WEBLAB"}) {
		t.Fatalf("owner teams %v", got.OwnerTeams)
	}
	if got.Events.Total != 3 || got.Events.Upcoming != 1 || got.Events.Live != 1 {
		t.Fatalf("event counts %+v", got.Events)
	}
	if got.Applications.Total != 56 || got.Applications.Members != 34 || got.Applications.Guests != 22 || got.Applications.CheckedIn != 7 {
		t.Fatalf("applications %+v", got.Applications)
	}
	for _, asked := range f.store.asked {
		for _, id := range asked {
			if id == f.gecekoduSoon.ID || id == f.noTeam.ID || id == f.archived.ID {
				t.Fatalf("the store was asked about an Event outside the caller's scope or archived: %v", id)
			}
		}
	}
	if got.Members != nil || got.MembersUnavailable {
		t.Fatalf("a leader may not read people: %+v", got.Members)
	}

	cases := map[string]struct {
		p     authz.Principal
		teams []string
		total int
	}{
		// Privileged: every current Event, the one without an Owner team too.
		"privileged": {privileged, []string{"GECEKODU", "WEBLAB"}, 5},
		// GECEKODU lets its members edit its Events, so they read its applicants.
		"GECEKODU member": {authz.Principal{ID: uuid.NewString(), Groups: []string{"/UYELER/GECEKODU"}}, []string{"GECEKODU"}, 1},
		// An ordinary WEBLAB member reads no applicant list.
		"WEBLAB member": {authz.Principal{ID: uuid.NewString(), Groups: []string{"/UYELER/ARGE/WEBLAB"}}, []string{}, 0},
		// A WEBLAB member granted certificate:issue reads WEBLAB's.
		"certificate issuer": {authz.Principal{ID: uuid.NewString(), Groups: []string{"/UYELER/ARGE/WEBLAB"}, Roles: []string{"certificate:issue"}}, []string{"WEBLAB"}, 3},
		"no groups":          {authz.Principal{ID: uuid.NewString()}, []string{}, 0},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := f.svc.Summary(t.Context(), tc.p)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(got.OwnerTeams, tc.teams) || got.Events.Total != tc.total {
				t.Fatalf("teams %v total %d", got.OwnerTeams, got.Events.Total)
			}
		})
	}
}

func TestSummaryCountsDaysAndMonthsInTheClubsTimeZone(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	got, err := f.svc.Summary(t.Context(), privileged)
	if err != nil {
		t.Fatal(err)
	}
	daily := got.Applications.Daily
	if len(daily) != dashboard.DailyDays || daily[0].Date != "2026-09-04" || daily[len(daily)-1].Date != "2026-10-03" {
		t.Fatalf("daily %v", daily)
	}
	if daily[len(daily)-1].Count != 3 || daily[len(daily)-3].Count != 1 || daily[0].Count != 5 {
		t.Fatalf("daily counts %v", daily)
	}
	// The window starts at midnight in Istanbul, 21:00 UTC the day before.
	if want := time.Date(2026, 9, 3, 21, 0, 0, 0, time.UTC); !f.store.since.Equal(want) {
		t.Fatalf("since %v, want %v", f.store.since, want)
	}
	months := got.Events.ByMonth
	if len(months) != dashboard.EventMonths || months[0].Month != "2026-05" || months[5].Month != "2026-10" {
		t.Fatalf("months %v", months)
	}
	// July: WebLab Spring; August: Genel Kurul; October: the other three.
	if months[2].Count != 1 || months[3].Count != 1 || months[5].Count != 3 {
		t.Fatalf("month counts %v", months)
	}
	if got.TimeZone != "Europe/Istanbul" || !got.GeneratedAt.Equal(now) {
		t.Fatalf("zone %q at %v", got.TimeZone, got.GeneratedAt)
	}
}

func TestSummaryListsRunningComingAndRecentEventsWithTheirApplications(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	got, err := f.svc.Summary(t.Context(), privileged)
	if err != nil {
		t.Fatal(err)
	}
	// WebLab Spring ended three months ago and Genel Kurul (no end, so half
	// a day) 40 days ago: both are out; the archived Event never shows.
	if names := eventNames(got.EventStats); !slices.Equal(names, []string{"WebLab Meetup", "WebLab Bootcamp", "Gece Kodu"}) {
		t.Fatalf("event stats %v", names)
	}
	live, soon, gk := got.EventStats[0], got.EventStats[1], got.EventStats[2]
	if !live.Live || soon.Live || gk.Live {
		t.Fatalf("live %v %v %v", live.Live, soon.Live, gk.Live)
	}
	if !live.HasApplicationForm || !live.HasCoverImage || live.CoverImageURL == "" || live.CheckedIn != 7 {
		t.Fatalf("live %+v", live)
	}
	if soon.HasApplicationForm || soon.HasCoverImage || soon.Applications != 46 || soon.Capacity != 50 || soon.Members != 30 || soon.Guests != 16 {
		t.Fatalf("soon %+v", soon)
	}
	if !gk.HasApplicationForm {
		t.Fatal("an extra form counts as an application form")
	}
	if len(soon.DailyApplications) != dashboard.EventDailyDays {
		t.Fatalf("daily %v", soon.DailyApplications)
	}
	if want := []int{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1, 0, 2}; !slices.Equal(soon.DailyApplications, want) {
		t.Fatalf("daily %v want %v", soon.DailyApplications, want)
	}
}

func putMember(t *testing.T, dir *identity.Memory, group string, p identity.Person) {
	t.Helper()
	dir.PutUser(p)
	if err := dir.AddMember(t.Context(), group, p.ID); err != nil {
		t.Fatal(err)
	}
}

func seedMembers(t *testing.T, f *fixture) (ada, bob, cem, dil, eda uuid.UUID) {
	t.Helper()
	for _, g := range []identity.Group{
		{ID: "u", Name: "UYELER", Path: "/UYELER"},
		{ID: "arge", Name: "ARGE", Path: "/UYELER/ARGE"},
		{ID: "weblab", Name: "WEBLAB", Path: "/UYELER/ARGE/WEBLAB"},
		{ID: "weblab-l", Name: "LIDERLER", Path: "/UYELER/ARGE/WEBLAB/LIDERLER"},
		{ID: "gk", Name: "GECEKODU", Path: "/UYELER/GECEKODU"},
		{ID: "other", Name: "MISAFIR", Path: "/MISAFIR"},
	} {
		f.dir.PutGroup(g)
	}
	ada, bob, cem, dil, eda = uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	putMember(t, f.dir, "weblab-l", identity.Person{ID: ada, FirstName: "Ada", LastName: "Lovelace", Email: "ada@example.com", CreatedAt: at(now.Add(-time.Hour))})
	if err := f.dir.AddMember(t.Context(), "gk", ada); err != nil {
		t.Fatal(err)
	}
	putMember(t, f.dir, "weblab", identity.Person{ID: bob, FirstName: "Bob", LastName: "B", Email: "bob@example.com", CreatedAt: at(now.AddDate(0, -1, 0))})
	putMember(t, f.dir, "u", identity.Person{ID: cem, FirstName: "Cem", LastName: "C", CreatedAt: at(now.AddDate(-2, 0, 0))})
	// Disabled in Keycloak.
	putMember(t, f.dir, "gk", identity.Person{ID: dil, FirstName: "Dil", LastName: "D", CreatedAt: at(now.Add(-2 * time.Hour))})
	if err := f.dir.DisableUser(t.Context(), dil); err != nil {
		t.Fatal(err)
	}
	// Being erased: core holds the deletion marker.
	putMember(t, f.dir, "gk", identity.Person{ID: eda, FirstName: "Eda", LastName: "E", CreatedAt: at(now.Add(-3 * time.Hour))})
	f.store.blocked[eda] = true
	// Not a Member: outside the UYELER tree.
	putMember(t, f.dir, "other", identity.Person{ID: uuid.New(), FirstName: "Fikret", CreatedAt: at(now)})
	return
}

// The Members section is the User read decision of the user list: only a
// caller who may read people gets it. It leaves out people disabled in
// Keycloak and people core is erasing, and answers no contact.
func TestSummaryMembersAreActiveMembersWithoutContact(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ada, bob, cem, _, _ := seedMembers(t, f)
	got, err := f.svc.Summary(t.Context(), privileged)
	if err != nil {
		t.Fatal(err)
	}
	m := got.Members
	if m == nil || got.MembersUnavailable {
		t.Fatalf("members %+v unavailable %v", m, got.MembersUnavailable)
	}
	if m.Active != 3 {
		t.Fatalf("active %d", m.Active)
	}
	if len(m.NewByMonth) != dashboard.MemberMonths || m.NewByMonth[0].Month != "2025-11" || m.NewByMonth[11].Month != "2026-10" {
		t.Fatalf("months %v", m.NewByMonth)
	}
	if m.NewByMonth[11].Count != 1 || m.NewByMonth[10].Count != 1 {
		t.Fatalf("month counts %v", m.NewByMonth)
	}
	if len(m.RecentJoiners) != 3 || m.RecentJoiners[0].ID != ada || m.RecentJoiners[1].ID != bob || m.RecentJoiners[2].ID != cem {
		t.Fatalf("joiners %+v", m.RecentJoiners)
	}
	if !slices.Equal(m.RecentJoiners[0].Teams, []string{"GECEKODU", "WEBLAB"}) || !slices.Equal(m.RecentJoiners[1].Teams, []string{"WEBLAB"}) || len(m.RecentJoiners[2].Teams) != 0 {
		t.Fatalf("teams %+v", m.RecentJoiners)
	}
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "@") || strings.Contains(string(raw), "email") || strings.Contains(string(raw), "phone") {
		t.Fatalf("the summary carries contact data: %s", raw)
	}

	usersReader := authz.Principal{ID: uuid.NewString(), Roles: []string{"users:read"}}
	if got, err := f.svc.Summary(t.Context(), usersReader); err != nil || got.Members == nil {
		t.Fatalf("users:read reads people: %+v %v", got.Members, err)
	}
}

// One read of the Members tree serves everyone for its TTL; an erasure
// started meanwhile shows at once, since the marker is read every time.
func TestSummaryMembersTreeIsReadOncePerTTL(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ada, _, _, _, _ := seedMembers(t, f)
	svc := f.service(t, time.Minute)
	reads := func() int {
		n := 0
		for _, op := range f.dir.Ops {
			if op == "Members" {
				n++
			}
		}
		return n
	}
	if _, err := svc.Summary(t.Context(), privileged); err != nil {
		t.Fatal(err)
	}
	first := reads()
	if first == 0 {
		t.Fatal("the tree was not read")
	}
	f.store.blocked[ada] = true
	f.clock = now.Add(59 * time.Second)
	got, err := svc.Summary(t.Context(), privileged)
	if err != nil {
		t.Fatal(err)
	}
	if reads() != first {
		t.Fatal("the tree was read again within its TTL")
	}
	if got.Members.Active != 2 || got.Members.RecentJoiners[0].ID == ada {
		t.Fatalf("an erasure waits for the cache: %+v", got.Members)
	}
	if !got.Members.AsOf.Equal(now) {
		t.Fatalf("asOf %v", got.Members.AsOf)
	}
	f.clock = now.Add(time.Minute)
	if _, err := svc.Summary(t.Context(), privileged); err != nil {
		t.Fatal(err)
	}
	if reads() == first {
		t.Fatal("the tree was not read again after its TTL")
	}
}

type failingDirectory struct{ dashboard.GroupDirectory }

func (failingDirectory) GetGroup(context.Context, string) (identity.Group, error) {
	return identity.Group{}, errors.New("keycloak down")
}

// Keycloak down costs the Members section, not the summary.
func TestSummaryWithoutTheDirectoryKeepsTheRest(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	svc, err := dashboard.NewService(dashboard.Options{
		Events: f.events, Store: f.store, Authz: authz.NewAuthorizer(authz.DefaultPolicy()),
		Directory: failingDirectory{}, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := svc.Summary(t.Context(), privileged)
	if err != nil {
		t.Fatal(err)
	}
	if got.Members != nil || !got.MembersUnavailable || got.Events.Total != 5 {
		t.Fatalf("members %+v unavailable %v events %+v", got.Members, got.MembersUnavailable, got.Events)
	}
	// A caller who may not read people is not told the directory is down.
	got, err = svc.Summary(t.Context(), leader)
	if err != nil || got.MembersUnavailable {
		t.Fatalf("unavailable %v err %v", got.MembersUnavailable, err)
	}
}

// With nothing covered the store is not asked and every list is empty,
// not null.
func TestSummaryOfNothingIsEmptyLists(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.store.asked = nil
	got, err := f.svc.Summary(t.Context(), authz.Principal{ID: uuid.NewString()})
	if err != nil {
		t.Fatal(err)
	}
	if len(f.store.asked) != 0 {
		t.Fatalf("store asked %v", f.store.asked)
	}
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"ownerTeams":[]`, `"eventStats":[]`, `"members":null`} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("%s lacks %s", raw, want)
		}
	}
	if len(got.Applications.Daily) != dashboard.DailyDays || len(got.Events.ByMonth) != dashboard.EventMonths {
		t.Fatalf("empty summary %+v", got)
	}
}
