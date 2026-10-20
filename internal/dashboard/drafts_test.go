package dashboard_test

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/dashboard"
	"github.com/skylab-kulubu/core-backend/internal/event"
)

// A draft is covered only for who may see it (its editors and a Privileged
// reader, ADR 0066), with its status; it is never upcoming or live, nor is
// a cancelled Event (PR #231 review B3).
func TestSummaryCoversDraftsOnlyForWhoMaySeeThem(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	draft, err := f.events.Create(t.Context(), event.Event{Name: "WebLab Taslak", OwnerTeam: "WEBLAB", Status: event.StatusDraft,
		StartDate: at(now.Add(-time.Hour)), EndDate: at(now.Add(time.Hour))})
	if err != nil {
		t.Fatal(err)
	}
	f.store.counts[draft.ID] = dashboard.TicketCounts{Applications: 2, Members: 2}
	issuer := authz.Principal{ID: uuid.NewString(), Groups: []string{"/UYELER/ARGE/WEBLAB"}, Roles: []string{"certificate:issue"}}
	dk := authz.Principal{ID: uuid.NewString(), Groups: []string{"/UYELER/DK"}}
	for name, tc := range map[string]struct {
		p                     authz.Principal
		total, upcoming, live int
		applications          int
		sees                  bool
	}{
		// The fixture's: 3 WEBLAB, 1 GECEKODU draft, 1 without a team.
		"YK":                 {privileged, 6, 1, 1, 61, true},
		"DK":                 {dk, 6, 1, 1, 61, true},
		"the team's Leader":  {leader, 4, 1, 1, 58, true},
		"certificate issuer": {issuer, 3, 1, 1, 56, false},
	} {
		t.Run(name, func(t *testing.T) {
			before := len(f.store.asked)
			got, err := f.svc.Summary(t.Context(), tc.p)
			if err != nil {
				t.Fatal(err)
			}
			if got.Events.Total != tc.total || got.Events.Upcoming != tc.upcoming || got.Events.Live != tc.live {
				t.Fatalf("events %+v", got.Events)
			}
			if got.Applications.Total != tc.applications {
				t.Fatalf("applications %+v", got.Applications)
			}
			var stat *dashboard.EventStat
			for i := range got.EventStats {
				if got.EventStats[i].ID == draft.ID {
					stat = &got.EventStats[i]
				}
			}
			if (stat != nil) != tc.sees {
				t.Fatalf("draft listed %v, want %v: %v", stat != nil, tc.sees, eventNames(got.EventStats))
			}
			if stat != nil && (stat.Status != event.StatusDraft || stat.Live || stat.Active) {
				t.Fatalf("draft stat %+v", *stat)
			}
			for _, asked := range f.store.asked[before:] {
				for _, id := range asked {
					if id == draft.ID && !tc.sees {
						t.Fatal("the store was asked about a draft the caller may not see")
					}
				}
			}
		})
	}
}
