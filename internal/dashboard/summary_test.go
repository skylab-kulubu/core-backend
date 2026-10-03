package dashboard_test

import (
	"context"
	"encoding/json"
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
	names   map[uuid.UUID]dashboard.Account
	asked   [][]uuid.UUID
	since   time.Time
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		counts:  map[uuid.UUID]dashboard.TicketCounts{},
		daily:   map[uuid.UUID]map[string]int{},
		blocked: map[uuid.UUID]bool{},
		names:   map[uuid.UUID]dashboard.Account{},
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

func (s *fakeStore) Accounts(_ context.Context, ids []uuid.UUID) (map[uuid.UUID]dashboard.Account, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[uuid.UUID]dashboard.Account{}
	for _, id := range ids {
		a, stored := s.names[id]
		if stored {
			a.Stored = true
		}
		if s.blocked[id] {
			a.Blocked = true
		}
		if stored || a.Blocked {
			out[id] = a
		}
	}
	return out, nil
}

func (s *fakeStore) block(id uuid.UUID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.blocked[id] = true
}

// testClock is the services' clock, read by the Members cache's background
// reads too.
type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = t
}

type fixture struct {
	events *event.MemoryStore
	store  *fakeStore
	dir    *identity.Memory
	clock  *testClock
	svc    dashboard.Service

	weblabSoon, weblabLive, weblabOld, gecekoduSoon, noTeam, archived event.Event
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{events: event.NewMemoryStore(), store: newFakeStore(), dir: identity.NewMemory(), clock: &testClock{t: now}}
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
		Directory: f.dir, MembersTTL: ttl, Now: f.clock.now,
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
