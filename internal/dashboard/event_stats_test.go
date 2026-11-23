package dashboard_test

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/dashboard"
	"github.com/skylab-kulubu/core-backend/internal/event"
)

// eventStatsFixture adds to the summary's fixture what its eventStats
// leaves out: an Event years old and two without a date.
func eventStatsFixture(t *testing.T) (f *fixture, old, undatedZ, undatedA, draft event.Event) {
	t.Helper()
	f = newFixture(t)
	create := func(e event.Event) event.Event {
		created, err := f.events.Create(t.Context(), e)
		if err != nil {
			t.Fatal(err)
		}
		return created
	}
	old = create(event.Event{Name: "WebLab 2019", OwnerTeam: "WEBLAB", StartDate: at(time.Date(2019, 3, 1, 9, 0, 0, 0, time.UTC))})
	undatedZ = create(event.Event{Name: "Zeta Atölye", OwnerTeam: "WEBLAB", FormURL: "https://skyl.app/zeta"})
	undatedA = create(event.Event{Name: "Alfa Söyleşi", OwnerTeam: "WEBLAB"})
	draft = create(event.Event{Name: "WebLab Taslak", OwnerTeam: "WEBLAB", Status: event.StatusDraft})
	f.store.counts[old.ID] = dashboard.TicketCounts{Applications: 120, Members: 100, Guests: 20, CheckedIn: 90}
	f.store.counts[undatedZ.ID] = dashboard.TicketCounts{Applications: 4, Guests: 4}
	return f, old, undatedZ, undatedA, draft
}

// The list covers what the summary covers but with no date limit: the long
// past and the undated Events come too, by start date with the undated
// ones last by name. The archived Event never shows.
func TestEventStatsListsEveryCoveredEventWhateverItsDate(t *testing.T) {
	t.Parallel()
	f, old, undatedZ, undatedA, _ := eventStatsFixture(t)
	got, err := f.svc.EventStats(t.Context(), privileged)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"WebLab 2019", "WebLab Spring", "Genel Kurul", "WebLab Meetup", "WebLab Bootcamp", "Gece Kodu",
		"Alfa Söyleşi", "WebLab Taslak", "Zeta Atölye"}
	if names := eventNames(got.EventStats); !slices.Equal(names, want) {
		t.Fatalf("event stats %v, want %v", names, want)
	}
	if got.TimeZone != dashboard.TimeZone || !got.GeneratedAt.Equal(now) {
		t.Fatalf("zone %q at %v", got.TimeZone, got.GeneratedAt)
	}
	byID := map[uuid.UUID]dashboard.EventStat{}
	for _, s := range got.EventStats {
		byID[s.ID] = s
		if len(s.DailyApplications) != dashboard.EventDailyDays {
			t.Fatalf("%s daily %v", s.Name, s.DailyApplications)
		}
	}
	if o := byID[old.ID]; o.Applications != 120 || o.Members != 100 || o.Guests != 20 || o.CheckedIn != 90 || o.Live || o.Active {
		t.Fatalf("old %+v", o)
	}
	// Undated: never live; active as published and never ended (ADR 0066).
	z := byID[undatedZ.ID]
	if z.StartDate != nil || z.Live || !z.Active || !z.HasApplicationForm || z.Applications != 4 {
		t.Fatalf("undated %+v", z)
	}
	if a := byID[undatedA.ID]; a.Applications != 0 || a.HasApplicationForm || slices.ContainsFunc(a.DailyApplications, func(n int) bool { return n != 0 }) {
		t.Fatalf("undated without tickets %+v", a)
	}
	// The same row the summary gives for an Event both list.
	if soon := byID[f.weblabSoon.ID]; !slices.Equal(soon.DailyApplications, []int{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1, 0, 2}) || soon.Capacity != 50 {
		t.Fatalf("soon %+v", soon)
	}
	// One read of each aggregate, for the covered Events alone, and the
	// days reach back EventDailyDays from midnight in Istanbul.
	if len(f.store.asked) != 2 {
		t.Fatalf("store asked %d times", len(f.store.asked))
	}
	for _, asked := range f.store.asked {
		if slices.Contains(asked, f.archived.ID) || len(asked) != 9 {
			t.Fatalf("asked about %v", asked)
		}
	}
	if want := time.Date(2026, 9, 19, 21, 0, 0, 0, time.UTC); !f.store.since.Equal(want) {
		t.Fatalf("since %v, want %v", f.store.since, want)
	}
}

// Scope is the summary's: a Leader sees their own team's Events, drafts
// included, and nothing of another team's; a certificate issuer sees the
// team's Events but not its drafts; anyone else gets an empty list, not
// null, and the store is not asked.
func TestEventStatsCoversWhatTheSummaryCovers(t *testing.T) {
	t.Parallel()
	f, _, _, _, draft := eventStatsFixture(t)
	issuer := authz.Principal{ID: uuid.NewString(), Groups: []string{"/UYELER/ARGE/WEBLAB"}, Roles: []string{"certificate:issue"}}
	for name, tc := range map[string]struct {
		p    authz.Principal
		want []string
	}{
		"the team's Leader": {leader, []string{"WebLab 2019", "WebLab Spring", "WebLab Meetup", "WebLab Bootcamp",
			"Alfa Söyleşi", "WebLab Taslak", "Zeta Atölye"}},
		"certificate issuer": {issuer, []string{"WebLab 2019", "WebLab Spring", "WebLab Meetup", "WebLab Bootcamp",
			"Alfa Söyleşi", "Zeta Atölye"}},
		"WEBLAB member": {authz.Principal{ID: uuid.NewString(), Groups: []string{"/UYELER/ARGE/WEBLAB"}}, []string{}},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := f.svc.EventStats(t.Context(), tc.p)
			if err != nil {
				t.Fatal(err)
			}
			if names := eventNames(got.EventStats); !slices.Equal(names, tc.want) {
				t.Fatalf("event stats %v, want %v", names, tc.want)
			}
			for _, s := range got.EventStats {
				if s.ID == draft.ID && (s.Status != event.StatusDraft || s.Active || s.Live) {
					t.Fatalf("draft %+v", s)
				}
			}
		})
	}

	f.store.asked = nil
	got, err := f.svc.EventStats(t.Context(), authz.Principal{ID: uuid.NewString()})
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
	if !strings.Contains(string(raw), `"eventStats":[]`) {
		t.Fatalf("%s lacks an empty eventStats", raw)
	}
}
