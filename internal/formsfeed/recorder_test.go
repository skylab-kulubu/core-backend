package formsfeed

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/events"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
)

type memVersions struct {
	mu   sync.Mutex
	kept map[string]int64
}

func (m *memVersions) Advance(_ context.Context, _ pgx.Tx, kind string, id uuid.UUID, version int64, orEqual bool) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.kept == nil {
		m.kept = map[string]int64{}
	}
	key := kind + "/" + id.String()
	if have, ok := m.kept[key]; ok && (have > version || (have == version && !orEqual)) {
		return false, nil
	}
	m.kept[key] = version
	return true, nil
}

type fakeEvents struct {
	events   []event.Event
	sessions []event.SessionFeedbackForm
}

func (f fakeEvents) ListIncludingDrafts(context.Context, string) ([]event.Event, error) {
	return f.events, nil
}

func (f fakeEvents) ListSessionFeedbackForms(context.Context, uuid.UUID) ([]event.SessionFeedbackForm, error) {
	return f.sessions, nil
}

type fakeReader struct {
	got   Response
	err   error
	calls int
}

func (f *fakeReader) Response(_ context.Context, formID, responseID uuid.UUID, _ ...string) (Response, error) {
	f.calls++
	if f.err != nil {
		return Response{}, f.err
	}
	got := f.got
	got.FormID, got.ResponseID = formID, responseID
	return got, nil
}

type harness struct {
	rec      *Recorder
	reader   *fakeReader
	reports  []ticket.FormResponse
	result   ticket.FormResponseResult
	err      error
	states   []string
	lines    []string
	versions *memVersions
}

var (
	applicationForm = uuid.MustParse("11111111-1111-4111-8111-111111111111")
	feedbackForm    = uuid.MustParse("22222222-2222-4222-8222-222222222222")
	sessionForm     = uuid.MustParse("33333333-3333-4333-8333-333333333333")
	unknownForm     = uuid.MustParse("44444444-4444-4444-8444-444444444444")
	responseID      = uuid.MustParse("55555555-5555-4555-8555-555555555555")
	personID        = uuid.MustParse("66666666-6666-4666-8666-666666666666")
)

func newHarness() *harness {
	h := &harness{reader: &fakeReader{}, versions: &memVersions{}}
	h.result = ticket.FormResponseResult{Outcome: ticket.FormResponseRecorded, TicketsWritten: 1, ListingEvents: 1}
	h.rec = &Recorder{
		Events: fakeEvents{
			events: []event.Event{
				{ID: uuid.New(), FormURL: "https://forms.example/" + applicationForm.String()},
				{ID: uuid.New(), FeedbackFormURL: "https://forms.example/" + feedbackForm.String()},
			},
			sessions: []event.SessionFeedbackForm{{URL: "https://forms.example/" + sessionForm.String()}},
		},
		Forms: h.reader,
		Record: func(_ context.Context, r ticket.FormResponse) (ticket.FormResponseResult, error) {
			h.reports = append(h.reports, r)
			return h.result, h.err
		},
		States: func(_ context.Context, formID uuid.UUID, open bool, closesAt *time.Time, _ time.Time) (int, error) {
			h.states = append(h.states, fmt.Sprintf("%s open=%t closes=%v", formID, open, closesAt != nil))
			return 2, nil
		},
		Versions: h.versions,
		Logf:     func(format string, args ...any) { h.lines = append(h.lines, fmt.Sprintf(format, args...)) },
	}
	return h
}

func cloudEvent(typ string, data any) events.Event {
	raw, _ := json.Marshal(data)
	return events.Event{
		ID: uuid.NewString(), Source: Source, Type: typ, DataSchema: "urn:skylab:schema:" + typ + ":1", Data: raw, Queue: Queue,
	}
}

func created(form uuid.UUID, status string, version int64, extra map[string]any) map[string]any {
	data := map[string]any{
		"formId": form, "responseId": responseID, "status": status, "userId": nil, "hasGuest": false,
		"submittedAt": "2026-11-20T09:15:00Z", "hiddenFields": map[string]any{}, "version": version,
	}
	for k, v := range extra {
		data[k] = v
	}
	return data
}

func (h *harness) handle(t *testing.T, e events.Event) error {
	t.Helper()
	return h.rec.handle(context.Background(), nil, e)
}

func TestSignedInAnswerGoesToTheTicketRuleWithItsSubmittedAtAndInvite(t *testing.T) {
	h := newHarness()
	err := h.handle(t, cloudEvent(TypeResponseCreated, created(applicationForm, StatusAccepted, 3, map[string]any{
		"userId": personID, "hiddenFields": map[string]any{"davet": "abc_DEF-123", "note": "has space", "n": 5},
	})))
	if err != nil {
		t.Fatal(err)
	}
	if len(h.reports) != 1 || h.reader.calls != 0 {
		t.Fatalf("reports %d, reads %d; want 1, 0", len(h.reports), h.reader.calls)
	}
	r := h.reports[0]
	if r.FormID != applicationForm || r.ResponseID != responseID || r.Status != ticket.FormResponseAccepted || r.UserID == nil || *r.UserID != personID {
		t.Fatalf("report %+v", r)
	}
	if r.SubmittedAt == nil || !r.SubmittedAt.Equal(time.Date(2026, 11, 20, 9, 15, 0, 0, time.UTC)) {
		t.Fatalf("submittedAt %v", r.SubmittedAt)
	}
	if r.Invite != "abc_DEF-123" || r.Guest != nil {
		t.Fatalf("invite %q guest %v", r.Invite, r.Guest)
	}
	if !strings.Contains(strings.Join(h.lines, "\n"), `"outcome":"recorded"`) {
		t.Fatalf("log lines %v", h.lines)
	}
}

func TestGuestAnswerIsReadFromFormsAndNeverLogged(t *testing.T) {
	h := newHarness()
	h.reader.got = Response{Status: StatusAccepted, HasGuest: true, Version: 3, Guest: &Guest{FirstName: "Ada", LastName: "Lovelace", Email: "ada@example.com"}}
	if err := h.handle(t, cloudEvent(TypeResponseCreated, created(applicationForm, StatusAccepted, 3, map[string]any{"hasGuest": true}))); err != nil {
		t.Fatal(err)
	}
	if h.reader.calls != 1 || len(h.reports) != 1 {
		t.Fatalf("reads %d reports %d", h.reader.calls, len(h.reports))
	}
	if g := h.reports[0].Guest; g == nil || g.Email != "ada@example.com" || g.FirstName != "Ada" {
		t.Fatalf("guest %+v", g)
	}
	for _, line := range h.lines {
		if strings.Contains(line, "ada@") || strings.Contains(line, "Lovelace") {
			t.Fatalf("a log line names the guest: %s", line)
		}
	}
}

func TestAGuestInTheEventIsIgnored(t *testing.T) {
	h := newHarness()
	h.reader.got = Response{Status: StatusAccepted, Version: 3, Guest: &Guest{FirstName: "A", LastName: "B", Email: "read@example.com"}}
	data := created(applicationForm, StatusAccepted, 3, map[string]any{"hasGuest": true, "guest": map[string]any{"firstName": "X", "lastName": "Y", "email": "event@example.com"}})
	if err := h.handle(t, cloudEvent(TypeResponseCreated, data)); err != nil {
		t.Fatal(err)
	}
	if h.reports[0].Guest.Email != "read@example.com" {
		t.Fatalf("guest %+v", h.reports[0].Guest)
	}
}

func TestPendingAndDeclinedAnswersAreNotRead(t *testing.T) {
	for _, status := range []string{StatusPending, StatusDeclined} {
		h := newHarness()
		h.result = ticket.FormResponseResult{Outcome: ticket.FormResponseNotAccepted}
		if err := h.handle(t, cloudEvent(TypeResponseCreated, created(applicationForm, status, 1, map[string]any{"hasGuest": true}))); err != nil {
			t.Fatal(err)
		}
		if h.reader.calls != 0 || len(h.reports) != 1 || string(h.reports[0].Status) != status {
			t.Fatalf("%s: reads %d reports %+v", status, h.reader.calls, h.reports)
		}
	}
}

func TestStatusChangeToAcceptedReadsTheAnswerForItsSubmittedAt(t *testing.T) {
	h := newHarness()
	submitted := time.Date(2026, 11, 1, 8, 0, 0, 0, time.UTC)
	h.reader.got = Response{Status: StatusAccepted, UserID: &personID, Version: 9, SubmittedAt: &submitted, HiddenFields: map[string]string{"davet": "code1"}}
	data := map[string]any{"formId": applicationForm, "responseId": responseID, "status": StatusAccepted, "userId": personID, "version": 7,
		"nextFormId": nil, "returnedQuestionCount": 0, "reviewedAt": "2026-11-02T10:00:00Z"}
	if err := h.handle(t, cloudEvent(TypeResponseStatusChanged, data)); err != nil {
		t.Fatal(err)
	}
	if h.reader.calls != 1 || h.reports[0].SubmittedAt == nil || !h.reports[0].SubmittedAt.Equal(submitted) || h.reports[0].Invite != "code1" {
		t.Fatalf("reads %d report %+v", h.reader.calls, h.reports)
	}
	// The read's newer version is kept: the event of version 8 is stale.
	h.handle(t, cloudEvent(TypeResponseStatusChanged, map[string]any{"formId": applicationForm, "responseId": responseID, "status": StatusDeclined, "userId": personID, "version": 8}))
	if len(h.reports) != 1 {
		t.Fatalf("a stale event reached the Ticket rule: %+v", h.reports)
	}
}

func TestOlderOrEqualVersionChangesNothing(t *testing.T) {
	h := newHarness()
	if err := h.handle(t, cloudEvent(TypeResponseStatusChanged, map[string]any{"formId": applicationForm, "responseId": responseID, "status": StatusDeclined, "userId": personID, "version": 5})); err != nil {
		t.Fatal(err)
	}
	for _, v := range []int64{5, 4} {
		if err := h.handle(t, cloudEvent(TypeResponseCreated, created(applicationForm, StatusAccepted, v, map[string]any{"userId": personID}))); err != nil {
			t.Fatal(err)
		}
	}
	if len(h.reports) != 1 || h.reports[0].Status != ticket.FormResponseDeclined {
		t.Fatalf("reports %+v", h.reports)
	}
	if !strings.Contains(strings.Join(h.lines, "\n"), `"outcome":"stale"`) {
		t.Fatalf("no stale line: %v", h.lines)
	}
	// A pull sees the version it already has and takes it again: the
	// Ticket rule is safe to repeat, and a form linked later needs it.
	outcome, err := h.rec.Response(context.Background(), nil, Origin{Via: ViaPull}, Response{FormID: applicationForm, ResponseID: responseID, Status: StatusDeclined, UserID: &personID, Version: 5}, true)
	if err != nil || outcome != string(ticket.FormResponseRecorded) || len(h.reports) != 2 {
		t.Fatalf("pull: %q %v %d", outcome, err, len(h.reports))
	}
}

func TestFormCoreDoesNotKnowIsUnrelatedAndNothingIsRead(t *testing.T) {
	h := newHarness()
	err := h.handle(t, cloudEvent(TypeResponseCreated, created(unknownForm, StatusAccepted, 1, map[string]any{"hasGuest": true})))
	if !errors.Is(err, events.ErrUnrelated) || h.reader.calls != 0 || len(h.reports) != 0 {
		t.Fatalf("err %v reads %d reports %d", err, h.reader.calls, len(h.reports))
	}
	if len(h.versions.kept) != 0 {
		t.Fatalf("a version was kept: %v", h.versions.kept)
	}
}

func TestFeedbackFormAnswersAreNotReadButCarryTheInvite(t *testing.T) {
	for _, form := range []uuid.UUID{feedbackForm, sessionForm} {
		h := newHarness()
		h.result = ticket.FormResponseResult{Outcome: ticket.FormResponseFeedback}
		data := created(form, StatusAccepted, 1, map[string]any{"hasGuest": true, "hiddenFields": map[string]any{"davet": "Zx9_-"}})
		if err := h.handle(t, cloudEvent(TypeResponseCreated, data)); err != nil {
			t.Fatal(err)
		}
		if h.reader.calls != 0 || len(h.reports) != 1 || h.reports[0].Invite != "Zx9_-" {
			t.Fatalf("%s: reads %d reports %+v", form, h.reader.calls, h.reports)
		}
	}
}

func TestPermanentAndTransientFailures(t *testing.T) {
	cases := []struct {
		name      string
		event     events.Event
		readErr   error
		recordErr error
		permanent bool
	}{
		{name: "another source", event: func() events.Event {
			e := cloudEvent(TypeResponseCreated, created(applicationForm, StatusAccepted, 1, nil))
			e.Source = "/skymail"
			return e
		}(), permanent: true},
		{name: "schema 2", event: func() events.Event {
			e := cloudEvent(TypeResponseCreated, created(applicationForm, StatusAccepted, 1, nil))
			e.DataSchema = "urn:skylab:schema:forms.response.created:2"
			return e
		}(), permanent: true},
		{name: "no version", event: cloudEvent(TypeResponseCreated, map[string]any{"formId": applicationForm, "responseId": responseID, "status": StatusAccepted}), permanent: true},
		{name: "bad status", event: cloudEvent(TypeResponseCreated, created(applicationForm, "Approved", 1, nil)), permanent: true},
		{name: "answer gone", event: cloudEvent(TypeResponseCreated, created(applicationForm, StatusAccepted, 1, map[string]any{"hasGuest": true})), readErr: ErrNotFound, permanent: true},
		{name: "Forms down", event: cloudEvent(TypeResponseCreated, created(applicationForm, StatusAccepted, 1, map[string]any{"hasGuest": true})), readErr: errors.New("formsfeed: Forms answered 503")},
		{name: "Forms forbids", event: cloudEvent(TypeResponseCreated, created(applicationForm, StatusAccepted, 1, map[string]any{"hasGuest": true})), readErr: ErrForbidden},
		{name: "rule refuses", event: cloudEvent(TypeResponseCreated, created(applicationForm, StatusAccepted, 1, map[string]any{"userId": personID})), recordErr: ticket.ErrInvalid, permanent: true},
		{name: "database", event: cloudEvent(TypeResponseCreated, created(applicationForm, StatusAccepted, 1, map[string]any{"userId": personID})), recordErr: errors.New("connection reset")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness()
			h.reader.err, h.err = tc.readErr, tc.recordErr
			err := h.handle(t, tc.event)
			if err == nil || errors.Is(err, events.ErrUnrelated) {
				t.Fatalf("err %v", err)
			}
			if events.IsPermanent(err) != tc.permanent {
				t.Fatalf("permanent %t, want %t: %v", events.IsPermanent(err), tc.permanent, err)
			}
		})
	}
}

func TestOtherFormsEventsAreUnrelated(t *testing.T) {
	h := newHarness()
	for _, typ := range []string{"forms.workflow_run.reopened", "forms.attempt.updated", "forms.something.new"} {
		if err := h.handle(t, cloudEvent(typ, map[string]any{"x": 1})); !errors.Is(err, events.ErrUnrelated) {
			t.Fatalf("%s: %v", typ, err)
		}
	}
}

func TestFormStateGoesToTheFeedbackRemindersInVersionOrder(t *testing.T) {
	h := newHarness()
	steps := []struct {
		data any
		want int
	}{
		{map[string]any{"formId": feedbackForm, "status": "closed", "closesAt": nil, "version": 4}, 1},
		{map[string]any{"formId": feedbackForm, "status": "open", "closesAt": "2026-12-01T00:00:00Z", "version": 3}, 1},
		{map[string]any{"formId": feedbackForm, "status": "open", "closesAt": "2026-12-01T00:00:00Z", "version": 5}, 2},
	}
	for _, s := range steps {
		if err := h.handle(t, cloudEvent(TypeFormUpdated, s.data)); err != nil {
			t.Fatal(err)
		}
		if len(h.states) != s.want {
			t.Fatalf("states %v", h.states)
		}
	}
	if h.states[0] != feedbackForm.String()+" open=false closes=false" || h.states[1] != feedbackForm.String()+" open=true closes=true" {
		t.Fatalf("states %v", h.states)
	}
	err := h.handle(t, cloudEvent(TypeFormUpdated, map[string]any{"formId": feedbackForm, "status": "archived", "version": 9}))
	if !events.IsPermanent(err) {
		t.Fatalf("unknown status: %v", err)
	}
}

func TestPrometheusListsTheCounters(t *testing.T) {
	h := newHarness()
	h.handle(t, cloudEvent(TypeResponseCreated, created(applicationForm, StatusAccepted, 1, map[string]any{"userId": personID})))
	out := h.rec.Prometheus()
	for _, want := range []string{
		`skylab_forms_answers_total{via="event",outcome="recorded"} 1`,
		`skylab_forms_answers_total{via="pull",outcome="stale"} 0`,
		`skylab_forms_answer_reads_total{result="ok"} 0`,
		`skylab_forms_form_states_total{outcome="processed"} 0`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %s in\n%s", want, out)
		}
	}
}
