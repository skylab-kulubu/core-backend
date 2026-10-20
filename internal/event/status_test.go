package event_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/event"
)

func at(t time.Time) *time.Time { return &t }

// statusCase is one row of the derivation table (ADR 0066): every stored
// status against an Event that ended, one still to come and an undated one.
type statusCase struct {
	name         string
	status       event.Status
	start, end   *time.Time
	active       bool
	ended        bool
	finalizeOpen bool
}

func statusCases(now time.Time) []statusCase {
	past := at(now.Add(-72 * time.Hour))
	pastEnd := at(now.Add(-48 * time.Hour))
	future := at(now.Add(48 * time.Hour))
	futureEnd := at(now.Add(72 * time.Hour))
	// Started 12 hours ago, no end date: it ends a day after its start.
	running := at(now.Add(-12 * time.Hour))
	// Started 30 hours ago, no end date: ended 6 hours ago.
	startedLongAgo := at(now.Add(-30 * time.Hour))
	return []statusCase{
		{"draft past", event.StatusDraft, past, pastEnd, false, true, false},
		{"draft future", event.StatusDraft, future, futureEnd, false, false, false},
		{"draft undated", event.StatusDraft, nil, nil, false, false, false},
		{"published past", event.StatusPublished, past, pastEnd, false, true, true},
		{"published future", event.StatusPublished, future, futureEnd, true, false, false},
		{"published undated", event.StatusPublished, nil, nil, true, false, true},
		{"published running, start only", event.StatusPublished, running, nil, true, false, false},
		{"published ended, start only", event.StatusPublished, startedLongAgo, nil, false, true, true},
		{"published end only, future", event.StatusPublished, nil, futureEnd, true, false, false},
		{"cancelled past", event.StatusCancelled, past, pastEnd, false, true, false},
		{"cancelled future", event.StatusCancelled, future, futureEnd, false, false, false},
		{"cancelled undated", event.StatusCancelled, nil, nil, false, false, false},
	}
}

func TestActiveIsDerivedFromStatusAndDates(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	for _, tc := range statusCases(now) {
		e := event.Event{Status: tc.status, StartDate: tc.start, EndDate: tc.end}
		if got := e.ActiveAt(now); got != tc.active {
			t.Errorf("%s: active = %v, want %v", tc.name, got, tc.active)
		}
		if got := e.EndedAt(now); got != tc.ended {
			t.Errorf("%s: ended = %v, want %v", tc.name, got, tc.ended)
		}
		if got := e.FinalizeOpenAt(now); got != tc.finalizeOpen {
			t.Errorf("%s: finalize open = %v, want %v", tc.name, got, tc.finalizeOpen)
		}
	}
}

func TestEndsAtIsTheEndDateElseADayAfterTheStart(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 3, 14, 19, 0, 0, 0, time.UTC)
	end := time.Date(2026, 3, 15, 3, 0, 0, 0, time.UTC)
	if got := (event.Event{StartDate: &start, EndDate: &end}).EndsAt(); got == nil || !got.Equal(end) {
		t.Fatalf("with end date: %v", got)
	}
	if got := (event.Event{StartDate: &start}).EndsAt(); got == nil || !got.Equal(start.Add(24*time.Hour)) {
		t.Fatalf("start only: %v", got)
	}
	if got := (event.Event{}).EndsAt(); got != nil {
		t.Fatalf("undated: %v", got)
	}
}

// The memory store answers the derived active, and the active list filters
// with it, like the database.
func TestMemoryStoreDerivesActive(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := event.NewMemoryStore()
	now := time.Now()
	want := map[uuid.UUID]statusCase{}
	for _, tc := range statusCases(now) {
		created, err := store.Create(ctx, event.Event{Name: tc.name, Location: "YTÜ", OwnerTeam: "WEBLAB", Status: tc.status, StartDate: tc.start, EndDate: tc.end})
		if err != nil {
			t.Fatal(err)
		}
		if created.Active != tc.active || created.Status != tc.status {
			t.Errorf("%s: created active=%v status=%q", tc.name, created.Active, created.Status)
		}
		want[created.ID] = tc
	}
	all, err := store.ListIncludingDrafts(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != len(want) {
		t.Fatalf("listed %d, want %d", len(all), len(want))
	}
	activeOnly, err := store.List(ctx, "", true)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range activeOnly {
		if !want[e.ID].active || !e.Active {
			t.Errorf("%s listed as active", e.Name)
		}
	}
	count := 0
	for _, tc := range want {
		if tc.active {
			count++
		}
	}
	if len(activeOnly) != count {
		t.Fatalf("active list %d, want %d", len(activeOnly), count)
	}
}

func TestCreateWithoutStatusPublishes(t *testing.T) {
	t.Parallel()
	_, svc := setup(t)
	ctx := context.Background()
	leaderID := uuid.New()
	leader := authz.Principal{ID: leaderID.String(), Groups: []string{"/UYELER/ARGE/WEBLAB/LIDERLER"}}
	before := time.Now().Add(-time.Second)

	created, err := svc.Create(ctx, leader, event.Event{Name: "Hack", Location: "YTÜ", OwnerTeam: "WEBLAB"})
	if err != nil {
		t.Fatal(err)
	}
	if created.Status != event.StatusPublished || !created.Active {
		t.Fatalf("status=%q active=%v", created.Status, created.Active)
	}
	if created.PublishedAt == nil || created.PublishedAt.Before(before) {
		t.Fatalf("publishedAt %v", created.PublishedAt)
	}
	if created.PublishedBy == nil || *created.PublishedBy != leaderID {
		t.Fatalf("publishedBy %v", created.PublishedBy)
	}
}

func TestCreateAsDraftIsNotActiveAndNotPublished(t *testing.T) {
	t.Parallel()
	_, svc := setup(t)
	ctx := context.Background()
	leader := authz.Principal{ID: uuid.NewString(), Groups: []string{"/UYELER/ARGE/WEBLAB/LIDERLER"}}

	created, err := svc.Create(ctx, leader, event.Event{Name: "Hack", Location: "YTÜ", OwnerTeam: "WEBLAB", Status: event.StatusDraft})
	if err != nil {
		t.Fatal(err)
	}
	if created.Status != event.StatusDraft || created.Active || created.PublishedAt != nil || created.PublishedBy != nil {
		t.Fatalf("draft %+v", created)
	}
}

func TestCreateRefusesCancelledOrUnknownStatus(t *testing.T) {
	t.Parallel()
	_, svc := setup(t)
	ctx := context.Background()
	leader := authz.Principal{ID: uuid.NewString(), Groups: []string{"/UYELER/ARGE/WEBLAB/LIDERLER"}}
	for _, status := range []event.Status{event.StatusCancelled, "archived", "PUBLISHED"} {
		if _, err := svc.Create(ctx, leader, event.Event{Name: "Hack", Location: "YTÜ", OwnerTeam: "WEBLAB", Status: status}); !errors.Is(err, event.ErrInvalid) {
			t.Fatalf("status %q: %v", status, err)
		}
	}
}

// An update writes neither the status nor active: a client that sends the
// whole body back (sky-app, today's panel) cannot hide, publish or cancel an
// Event with it.
func TestUpdateKeepsTheStatusWhateverTheBodySays(t *testing.T) {
	t.Parallel()
	_, svc := setup(t)
	ctx := context.Background()
	leader := authz.Principal{ID: uuid.NewString(), Groups: []string{"/UYELER/ARGE/WEBLAB/LIDERLER"}}

	published, err := svc.Create(ctx, leader, event.Event{Name: "Hack", Location: "YTÜ", OwnerTeam: "WEBLAB"})
	if err != nil {
		t.Fatal(err)
	}
	updated, err := svc.Update(ctx, leader, published.ID, event.Event{Name: "Hack 2", Location: "YTÜ", OwnerTeam: "WEBLAB", Active: false, Status: event.StatusDraft})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Status != event.StatusPublished || !updated.Active || updated.Name != "Hack 2" {
		t.Fatalf("updated %+v", updated)
	}
	if updated.PublishedAt == nil || !updated.PublishedAt.Equal(*published.PublishedAt) {
		t.Fatalf("publishedAt %v, was %v", updated.PublishedAt, published.PublishedAt)
	}

	draft, err := svc.Create(ctx, leader, event.Event{Name: "Taslak", Location: "YTÜ", OwnerTeam: "WEBLAB", Status: event.StatusDraft})
	if err != nil {
		t.Fatal(err)
	}
	updated, err = svc.Update(ctx, leader, draft.ID, event.Event{Name: "Taslak", Location: "YTÜ", OwnerTeam: "WEBLAB", Active: true, Status: event.StatusPublished})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Status != event.StatusDraft || updated.Active || updated.PublishedAt != nil {
		t.Fatalf("draft after update %+v", updated)
	}
}
