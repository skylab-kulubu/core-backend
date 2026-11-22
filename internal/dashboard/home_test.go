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
	"github.com/skylab-kulubu/core-backend/internal/season"
)

// fakeHome answers fixed figures and records which Events it was asked
// about.
type fakeHome struct {
	mu       sync.Mutex
	sessions map[uuid.UUID]int
	slots    []dashboard.SessionSlot
	failed   map[uuid.UUID]int
	requests dashboard.SubjectRequestCounts
	checkIns map[uuid.UUID]map[string]int
	certs    dashboard.CertificateCounts
	asked    []uuid.UUID
	certAsk  []uuid.UUID
	today    string
}

func (h *fakeHome) ask(ids []uuid.UUID) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.asked = append(h.asked, ids...)
}

func (h *fakeHome) SessionCounts(_ context.Context, ids []uuid.UUID) (map[uuid.UUID]int, error) {
	h.ask(ids)
	return h.sessions, nil
}

func (h *fakeHome) SessionsBetween(_ context.Context, ids []uuid.UUID, from, to time.Time, limit int) ([]dashboard.SessionSlot, error) {
	h.ask(ids)
	out := make([]dashboard.SessionSlot, 0)
	for _, slot := range h.slots {
		if slices.Contains(ids, slot.EventID) && !slot.StartTime.Before(from) && slot.StartTime.Before(to) && len(out) < limit {
			out = append(out, slot)
		}
	}
	return out, nil
}

func (h *fakeHome) FailedCertificateJobs(_ context.Context, ids []uuid.UUID) (map[uuid.UUID]int, error) {
	h.ask(ids)
	out := map[uuid.UUID]int{}
	for _, id := range ids {
		if n, ok := h.failed[id]; ok {
			out[id] = n
		}
	}
	return out, nil
}

func (h *fakeHome) OpenSubjectRequests(_ context.Context, today string) (dashboard.SubjectRequestCounts, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.today = today
	return h.requests, nil
}

func (h *fakeHome) DailyCheckIns(_ context.Context, ids []uuid.UUID, _ time.Time, _ *time.Location) (map[uuid.UUID]map[string]int, error) {
	h.ask(ids)
	out := map[uuid.UUID]map[string]int{}
	for _, id := range ids {
		if d, ok := h.checkIns[id]; ok {
			out[id] = d
		}
	}
	return out, nil
}

func (h *fakeHome) CertificateCounts(_ context.Context, ids []uuid.UUID, _, _ time.Time) (dashboard.CertificateCounts, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.certAsk = append(h.certAsk, ids...)
	return h.certs, nil
}

type fakeSeasons []season.Season

func (f fakeSeasons) List(context.Context, bool) ([]season.Season, error) { return f, nil }

type homeFixture struct {
	*fixture
	home *fakeHome
	svc  dashboard.Service
	// tomorrow is a published WEBLAB Event starting tomorrow without a
	// place; gecekoduWeek a published GECEKODU one in three days;
	// weblabDraft an undated WEBLAB draft.
	tomorrow, gecekoduWeek, weblabDraft event.Event
	season                              uuid.UUID
}

func newHomeFixture(t *testing.T) *homeFixture {
	t.Helper()
	f := &homeFixture{fixture: newFixture(t), home: &fakeHome{
		sessions: map[uuid.UUID]int{},
		failed:   map[uuid.UUID]int{},
		requests: dashboard.SubjectRequestCounts{Open: 2, Overdue: 1},
		checkIns: map[uuid.UUID]map[string]int{},
		certs:    dashboard.CertificateCounts{Issued: 10, Revoked: 1, Period: dashboard.Period{Current: 3, Previous: 2}},
	}}
	running := season.Season{ID: uuid.New(), Name: "2026-2027", StartDate: at(time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)), EndDate: at(time.Date(2027, 6, 30, 0, 0, 0, 0, time.UTC))}
	old := season.Season{ID: uuid.New(), Name: "2025-2026", StartDate: at(time.Date(2025, 9, 1, 0, 0, 0, 0, time.UTC)), EndDate: at(time.Date(2026, 6, 30, 0, 0, 0, 0, time.UTC))}
	undated := season.Season{ID: uuid.New(), Name: "Tarihsiz"}
	f.season = running.ID
	create := func(e event.Event) event.Event {
		created, err := f.events.Create(t.Context(), e)
		if err != nil {
			t.Fatal(err)
		}
		return created
	}
	f.tomorrow = create(event.Event{Name: "WebLab Atölye", OwnerTeam: "WEBLAB", FormURL: "https://skyl.app/atolye",
		StartDate: at(now.Add(24 * time.Hour)), SeasonID: &running.ID})
	f.gecekoduWeek = create(event.Event{Name: "Gece Atölye", OwnerTeam: "GECEKODU", Location: "B-1",
		StartDate: at(now.Add(3 * 24 * time.Hour))})
	f.weblabDraft = create(event.Event{Name: "WebLab Fikir", OwnerTeam: "WEBLAB", Status: event.StatusDraft, SeasonID: &running.ID})
	f.home.checkIns[f.weblabLive.ID] = map[string]int{"2026-10-03": 4, "2026-09-01": 2}
	// The Meetup running now has a Session; tomorrow's has none.
	f.home.sessions[f.weblabLive.ID] = 1
	f.home.slots = []dashboard.SessionSlot{
		{ID: uuid.New(), EventID: f.weblabLive.ID, Title: "Açılış", StartTime: now.Add(-time.Hour)},
		{ID: uuid.New(), EventID: f.gecekoduWeek.ID, Title: "Kurulum", StartTime: now.Add(3 * 24 * time.Hour)},
		// Next week: out of the week.
		{ID: uuid.New(), EventID: f.weblabSoon.ID, Title: "Sonra", StartTime: now.Add(8 * 24 * time.Hour)},
	}
	f.home.failed[f.weblabOld.ID] = 3
	f.home.failed[f.gecekoduSoon.ID] = 5
	svc, err := dashboard.NewService(dashboard.Options{
		Events: f.events, Store: f.store, Home: f.home, Authz: authz.NewAuthorizer(authz.DefaultPolicy()),
		Directory: f.dir, Now: f.clock.now, Seasons: fakeSeasons{old, undated, running},
	})
	if err != nil {
		t.Fatal(err)
	}
	f.svc = svc
	return f
}

func todoNames(events []dashboard.TodoEvent) []string {
	out := make([]string, len(events))
	for i, e := range events {
		out[i] = e.Name
	}
	return out
}

func weekNames(w dashboard.Week) (events, sessions []string) {
	for _, e := range w.Events {
		events = append(events, e.Name)
	}
	for _, s := range w.Sessions {
		sessions = append(sessions, s.EventName+"/"+s.Title)
	}
	return events, sessions
}

// A Leader's to-dos are their own team's: the drafts to publish, the Events
// of today and tomorrow that still lack a place, a form or a Session, the
// failed certificates; no data subject request. Their week is the club's
// published Events; their figures their team's.
func TestHomeOfALeader(t *testing.T) {
	t.Parallel()
	f := newHomeFixture(t)
	got, err := f.svc.Summary(t.Context(), leader)
	if err != nil {
		t.Fatal(err)
	}
	h := got.Home
	if h == nil || h.Todo == nil {
		t.Fatalf("home %+v", h)
	}
	if names := todoNames(h.Todo.Drafts); !slices.Equal(names, []string{"WebLab Fikir"}) {
		t.Fatalf("drafts %v", names)
	}
	// The Meetup runs now with a place, a form and a Session: nothing is
	// missing. The Bootcamp starts in five days: not yet a to-do.
	if len(h.Todo.Incomplete) != 1 {
		t.Fatalf("incomplete %+v", h.Todo.Incomplete)
	}
	for _, item := range h.Todo.Incomplete {
		switch item.Name {
		case "WebLab Atölye":
			if !slices.Equal(item.Missing, []string{dashboard.MissingLocation, dashboard.MissingSessions}) || item.Live {
				t.Fatalf("tomorrow %+v", item)
			}
		case "WebLab Meetup":
			t.Fatalf("the Meetup lacks nothing: %+v", item)
		}
	}
	if fc := h.Todo.FailedCertificates; len(fc) != 1 || fc[0].EventID != f.weblabOld.ID || fc[0].Failed != 3 {
		t.Fatalf("failed certificates %+v", fc)
	}
	if h.Todo.SubjectRequests != nil {
		t.Fatal("a Leader handles no data subject request")
	}
	events, sessions := weekNames(h.Week)
	if !slices.Equal(events, []string{"WebLab Meetup", "WebLab Atölye", "Gece Atölye", "WebLab Bootcamp"}) {
		t.Fatalf("week events %v", events)
	}
	if !slices.Equal(sessions, []string{"WebLab Meetup/Açılış", "Gece Atölye/Kurulum"}) {
		t.Fatalf("week sessions %v", sessions)
	}
	if !h.Week.Events[0].Live || h.Week.Events[1].Live {
		t.Fatalf("live flags %+v", h.Week.Events)
	}
	if h.Week.From.Format(time.RFC3339) != "2026-10-02T21:00:00Z" || h.Week.To.Format(time.RFC3339) != "2026-10-09T21:00:00Z" {
		t.Fatalf("week %v – %v", h.Week.From, h.Week.To)
	}
	// The last 30 days are 4 Sep to 3 Oct; the 30 before, 5 Aug to 3 Sep.
	c := h.Compare
	if c == nil || c.Days != 30 || c.NewMembers != nil ||
		c.Applications != (dashboard.Period{Current: 8, Previous: 100}) || c.Attendees != (dashboard.Period{Current: 4, Previous: 2}) {
		t.Fatalf("compare %+v", c)
	}
	// The Meetup started two hours ago; the Spring three months ago.
	if c.Events != (dashboard.Period{Current: 1}) {
		t.Fatalf("events compare %+v", c.Events)
	}
	if len(h.Daily) != 60 || h.Daily[59] != (dashboard.DailyActivity{Date: "2026-10-03", Applications: 2, Attendees: 4}) || h.Daily[0].Date != "2026-08-05" {
		t.Fatalf("daily %+v … %+v", h.Daily[0], h.Daily[len(h.Daily)-1])
	}
	if len(h.Monthly) != 12 || h.Monthly[0].Month != "2025-11" ||
		h.Monthly[11] != (dashboard.MonthlyActivity{Month: "2026-10", Applications: 3, Attendees: 4, Events: 1}) ||
		h.Monthly[10] != (dashboard.MonthlyActivity{Month: "2026-09", Applications: 105, Attendees: 2}) ||
		h.Monthly[8].Events != 1 {
		t.Fatalf("monthly %+v", h.Monthly)
	}
	if len(h.Teams) != 1 || h.Teams[0] != (dashboard.TeamActivity{Team: "WEBLAB", Events: 2, Applications: 108, Attendees: 6}) {
		t.Fatalf("teams %+v", h.Teams)
	}
	if h.Sessions != 1 || h.Certificates == nil || *h.Certificates != f.home.certs {
		t.Fatalf("sessions %d certificates %+v", h.Sessions, h.Certificates)
	}
	// The running season with its published Events: not the draft.
	if h.Season == nil || h.Season.ID != f.season || len(h.Season.Events) != 1 || h.Season.Events[0].Name != "WebLab Atölye" {
		t.Fatalf("season %+v", h.Season)
	}
	// The club's published Events, whoever owns them, by month and team.
	if h.Club.EventsByMonth[11] != (dashboard.MonthCount{Month: "2026-10", Count: 4}) ||
		!slices.Equal(h.Club.EventsByTeam, []dashboard.TeamCount{{Team: "WEBLAB", Count: 4}, {Team: "", Count: 1}, {Team: "GECEKODU", Count: 1}}) {
		t.Fatalf("club %+v", h.Club)
	}
	for _, id := range f.home.asked {
		if id == f.gecekoduSoon.ID {
			t.Fatal("the store was asked about another team's draft")
		}
	}
}

// YK acts on every team and handles the data subject requests; DK reads
// everything but acts on nothing, so it has no to-do list at all.
func TestHomeOfTheBoard(t *testing.T) {
	t.Parallel()
	f := newHomeFixture(t)
	// Ada (an hour ago) and Bob (a month ago, at the periods' border) are
	// new; Fen came 45 days ago; Cem two years ago; Dil and Eda do not
	// count (disabled, being erased).
	seedMembers(t, f.fixture)
	putMember(t, f.dir, "u", identity.Person{ID: uuid.New(), FirstName: "Fen", LastName: "F", CreatedAt: at(now.Add(-45 * 24 * time.Hour))})

	yk, err := f.svc.Summary(t.Context(), privileged)
	if err != nil {
		t.Fatal(err)
	}
	todo := yk.Home.Todo
	if todo == nil || !slices.Equal(todoNames(todo.Drafts), []string{"Gece Kodu", "WebLab Fikir"}) {
		t.Fatalf("YK todo %+v", todo)
	}
	if todo.SubjectRequests == nil || *todo.SubjectRequests != f.home.requests || f.home.today != "2026-10-03" {
		t.Fatalf("subject requests %+v (today %q)", todo.SubjectRequests, f.home.today)
	}
	if len(todo.FailedCertificates) != 2 {
		t.Fatalf("failed certificates %+v", todo.FailedCertificates)
	}
	if nm := yk.Home.Compare.NewMembers; nm == nil || *nm != (dashboard.Period{Current: 2, Previous: 1}) {
		t.Fatalf("new members %+v", nm)
	}

	dk := authz.Principal{ID: uuid.NewString(), Groups: []string{"/UYELER/DK"}}
	got, err := f.svc.Summary(t.Context(), dk)
	if err != nil {
		t.Fatal(err)
	}
	if got.Home == nil || got.Home.Todo != nil || got.Home.Compare == nil || got.Home.Compare.NewMembers == nil {
		t.Fatalf("DK home %+v", got.Home)
	}
}

// A Member who manages nothing has no to-dos and no figures, only the
// club's week.
func TestHomeOfAMember(t *testing.T) {
	t.Parallel()
	f := newHomeFixture(t)
	member := authz.Principal{ID: uuid.NewString(), Groups: []string{"/UYELER/ARGE/WEBLAB"}}
	got, err := f.svc.Summary(t.Context(), member)
	if err != nil {
		t.Fatal(err)
	}
	if got.Home == nil || got.Home.Todo != nil || got.Home.Compare != nil {
		t.Fatalf("member home %+v", got.Home)
	}
	if events, _ := weekNames(got.Home.Week); len(events) != 4 {
		t.Fatalf("member week %v", events)
	}
	h := got.Home
	if h.Season == nil || len(h.Club.EventsByTeam) == 0 || len(h.Daily) != 0 || len(h.Teams) != 0 || h.Certificates != nil {
		t.Fatalf("member charts %+v", h)
	}
	body, err := json.Marshal(h)
	if err != nil || !strings.Contains(string(body), `"daily":[]`) {
		t.Fatalf("member json %s %v", body, err)
	}
	if f.home.today != "" {
		t.Fatal("the data subject requests were read for a Member")
	}
}

// Without a HomeStore the answer is the summary of before: no "home" key,
// which the panel reads as an older core.
func TestHomeLeftOutWithoutItsStore(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	got, err := f.svc.Summary(t.Context(), privileged)
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), `"home"`) {
		t.Fatalf("home answered without a store: %s", body)
	}
}

// A product's service account is not a person: no home page.
func TestHomeNotForAServiceAccount(t *testing.T) {
	t.Parallel()
	f := newHomeFixture(t)
	got, err := f.svc.Summary(t.Context(), authz.Principal{ID: uuid.NewString(), Product: "forms", Groups: []string{"/UYELER/YK"}})
	if err != nil {
		t.Fatal(err)
	}
	if got.Home != nil {
		t.Fatalf("service account home %+v", got.Home)
	}
}
