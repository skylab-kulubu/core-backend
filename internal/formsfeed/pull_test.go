package formsfeed

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
)

type memQueue struct {
	jobs       []PullJob
	cursors    []string
	finished   []*time.Time
	retries    []time.Time
	attempts   []int
	dropped    int
	planned    [][]uuid.UUID
	backfilled map[uuid.UUID]bool
}

func (q *memQueue) Backfill(_ context.Context, formID, eventID, _ uuid.UUID, _ time.Time) (bool, error) {
	if q.backfilled == nil {
		q.backfilled = map[uuid.UUID]bool{}
	}
	if q.backfilled[formID] {
		return false, nil
	}
	q.backfilled[formID] = true
	return true, nil
}

func (q *memQueue) Claim(context.Context, time.Time) (PullJob, bool, error) {
	if len(q.jobs) == 0 {
		return PullJob{}, false, nil
	}
	j := q.jobs[0]
	q.jobs = q.jobs[1:]
	return j, true, nil
}

func (q *memQueue) SaveCursor(_ context.Context, _ uuid.UUID, cursor string) error {
	q.cursors = append(q.cursors, cursor)
	return nil
}

func (q *memQueue) Finish(_ context.Context, _ PullJob, watermark *time.Time, _ time.Time) error {
	q.finished = append(q.finished, watermark)
	return nil
}

func (q *memQueue) Retry(_ context.Context, _ uuid.UUID, attempts int, next time.Time) error {
	q.attempts = append(q.attempts, attempts)
	q.retries = append(q.retries, next)
	return nil
}

func (q *memQueue) Drop(context.Context, uuid.UUID) error { q.dropped++; return nil }

func (q *memQueue) Plan(_ context.Context, forms []uuid.UUID, _ time.Time) (int, error) {
	q.planned = append(q.planned, forms)
	return len(forms), nil
}

type pageLister struct {
	pages   map[string]Page
	err     error
	queries []ListQuery
}

func (l *pageLister) List(_ context.Context, _ uuid.UUID, q ListQuery) (Page, error) {
	l.queries = append(l.queries, q)
	if l.err != nil {
		return Page{}, l.err
	}
	return l.pages[q.After], nil
}

var pullNow = time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)

func newPuller(h *harness, q *memQueue, l *pageLister) *Puller {
	return &Puller{Queue: q, Forms: l, Recorder: h.rec, Events: h.rec.Events, Now: func() time.Time { return pullNow }, Logf: h.rec.Logf}
}

func item(userID uuid.UUID, version int64) Response {
	id := userID
	return Response{FormID: applicationForm, ResponseID: uuid.New(), Status: StatusAccepted, UserID: &id, Version: version}
}

func TestBackfillPullsEveryPageThroughTheTicketRule(t *testing.T) {
	h := newHarness()
	h.reader.got = Response{Status: StatusAccepted, Version: 1, Guest: &Guest{FirstName: "A", LastName: "B", Email: "g@example.com"}}
	watermark := pullNow.Add(-time.Minute)
	guest := Response{FormID: applicationForm, ResponseID: uuid.New(), Status: StatusAccepted, HasGuest: true, Version: 1}
	l := &pageLister{pages: map[string]Page{
		"":   {Items: []Response{item(uuid.New(), 1), guest}, NextCursor: "p2"},
		"p2": {Items: []Response{item(uuid.New(), 2)}, Skipped: 1, Watermark: &watermark},
	}}
	event := uuid.New()
	q := &memQueue{jobs: []PullJob{{ID: uuid.New(), FormID: applicationForm, Kind: PullBackfill, EventID: &event, RequestedAt: pullNow}}}
	ran, err := newPuller(h, q, l).RunOne(context.Background())
	if err != nil || !ran {
		t.Fatalf("%t %v", ran, err)
	}
	if len(h.reports) != 3 || h.reader.calls != 1 || h.reports[1].Guest == nil {
		t.Fatalf("reports %+v reads %d", h.reports, h.reader.calls)
	}
	if l.queries[0].ChangedSince != nil || len(q.cursors) != 1 || q.cursors[0] != "p2" {
		t.Fatalf("queries %+v cursors %v", l.queries, q.cursors)
	}
	if len(q.finished) != 1 || q.finished[0] == nil || !q.finished[0].Equal(watermark) {
		t.Fatalf("finished %v", q.finished)
	}
	log := strings.Join(h.lines, "\n")
	if !strings.Contains(log, `"event":"form_pull"`) || !strings.Contains(log, `"tickets_written":3`) || !strings.Contains(log, `"skipped":1`) || strings.Contains(log, "g@example.com") {
		t.Fatalf("log %s", log)
	}
}

func TestCatchupAsksFromItsChangedSince(t *testing.T) {
	h := newHarness()
	since := pullNow.Add(-24*time.Hour - WatermarkMargin)
	l := &pageLister{pages: map[string]Page{"": {}}}
	q := &memQueue{jobs: []PullJob{{ID: uuid.New(), FormID: applicationForm, Kind: PullCatchup, ChangedSince: &since, RequestedAt: pullNow}}}
	if _, err := newPuller(h, q, l).RunOne(context.Background()); err != nil {
		t.Fatal(err)
	}
	if l.queries[0].ChangedSince == nil || !l.queries[0].ChangedSince.Equal(since) || len(q.finished) != 1 || q.finished[0] != nil {
		t.Fatalf("queries %+v finished %v", l.queries, q.finished)
	}
}

func TestAMissingListRouteWaitsLongerEachTimeAndGivesUpAfterAWeek(t *testing.T) {
	h := newHarness()
	l := &pageLister{err: ErrNotFound}
	for attempts, want := range map[int]time.Duration{0: time.Minute, 1: 2 * time.Minute, 4: 16 * time.Minute, 11: 24 * time.Hour, 30: 24 * time.Hour} {
		q := &memQueue{jobs: []PullJob{{ID: uuid.New(), FormID: applicationForm, Kind: PullCatchup, ChangedSince: &pullNow, RequestedAt: pullNow.Add(-time.Hour), Attempts: attempts}}}
		if _, err := newPuller(h, q, l).RunOne(context.Background()); err != nil {
			t.Fatal(err)
		}
		if len(q.retries) != 1 || q.retries[0].Sub(pullNow) != want || q.attempts[0] != attempts+1 {
			t.Fatalf("attempts %d: retries %v (%v)", attempts, q.retries, q.attempts)
		}
	}
	q := &memQueue{jobs: []PullJob{{ID: uuid.New(), FormID: applicationForm, Kind: PullBackfill, RequestedAt: pullNow.Add(-pullGiveUp), Attempts: 12}}}
	if _, err := newPuller(h, q, l).RunOne(context.Background()); err != nil {
		t.Fatal(err)
	}
	if q.dropped != 1 || len(q.retries) != 0 {
		t.Fatalf("dropped %d retries %v", q.dropped, q.retries)
	}
	if !strings.Contains(strings.Join(h.lines, "\n"), `"outcome":"abandoned","reason":"not_found"`) {
		t.Fatalf("log %v", h.lines)
	}
}

func TestATransientAnswerStopsTheJobAPermanentOneIsPassed(t *testing.T) {
	h := newHarness()
	h.err = ticket.ErrInvalid
	l := &pageLister{pages: map[string]Page{"": {Items: []Response{item(uuid.New(), 1), item(uuid.New(), 1)}}}}
	q := &memQueue{jobs: []PullJob{{ID: uuid.New(), FormID: applicationForm, Kind: PullBackfill, RequestedAt: pullNow}}}
	newPuller(h, q, l).RunOne(context.Background())
	if len(h.reports) != 2 || len(q.finished) != 1 {
		t.Fatalf("permanent: reports %d finished %d", len(h.reports), len(q.finished))
	}

	h = newHarness()
	h.err = errors.New("connection reset")
	q = &memQueue{jobs: []PullJob{{ID: uuid.New(), FormID: applicationForm, Kind: PullBackfill, RequestedAt: pullNow}}}
	newPuller(h, q, l).RunOne(context.Background())
	if len(h.reports) != 1 || len(q.finished) != 0 || len(q.retries) != 1 {
		t.Fatalf("transient: reports %d finished %d retries %d", len(h.reports), len(q.finished), len(q.retries))
	}
}

func TestPlanCoversTheOpenEventsApplicationForms(t *testing.T) {
	h := newHarness()
	extra, cancelledForm, endedForm, archivedForm := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	past := pullNow.Add(-48 * time.Hour)
	archived := pullNow.Add(-time.Hour)
	h.rec.Events = fakeEvents{events: []event.Event{
		{ID: uuid.New(), FormURL: "https://forms.example/" + applicationForm.String(), ExtraFormURLs: []event.EventFormLink{{URL: "https://forms.example/" + extra.String()}},
			FeedbackFormURL: "https://forms.example/" + feedbackForm.String()},
		{ID: uuid.New(), FormURL: "https://forms.example/" + cancelledForm.String(), Status: event.StatusCancelled},
		{ID: uuid.New(), FormURL: "https://forms.example/" + endedForm.String(), StartDate: &past, EndDate: &past},
		{ID: uuid.New(), FormURL: "https://forms.example/" + archivedForm.String(), ArchivedAt: &archived},
		{ID: uuid.New(), FormURL: "https://forms.example/" + applicationForm.String()},
	}}
	q := &memQueue{}
	p := newPuller(h, q, &pageLister{})
	p.Events = h.rec.Events
	if _, err := p.Plan(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := map[uuid.UUID]bool{}
	for _, id := range q.planned[0] {
		got[id] = true
	}
	if len(q.planned[0]) != 2 || !got[applicationForm] || !got[extra] {
		t.Fatalf("planned %v", q.planned[0])
	}
}
