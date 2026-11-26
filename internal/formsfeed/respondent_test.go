package formsfeed

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
)

func TestMappedGuest(t *testing.T) {
	split := Mapping{FirstName: "f", LastName: "l", Email: "e"}
	full := Mapping{FullName: "n", Email: "e"}
	both := Mapping{FirstName: "f", FullName: "n", Email: "e"}
	for name, tc := range map[string]struct {
		m       Mapping
		answers map[string]string
		want    ticket.GuestInfo
		ok      bool
	}{
		"first and last":            {split, map[string]string{"f": " Ada ", "l": "Lovelace", "e": " Ada@Example.COM "}, ticket.GuestInfo{FirstName: "Ada", LastName: "Lovelace", Email: "ada@example.com"}, true},
		"full name, last word last": {full, map[string]string{"n": "Ada  Augusta   King", "e": "ada@example.com"}, ticket.GuestInfo{FirstName: "Ada Augusta", LastName: "King", Email: "ada@example.com"}, true},
		"full fills the missing":    {both, map[string]string{"f": "Augusta", "n": "Ada Lovelace", "e": "ada@example.com"}, ticket.GuestInfo{FirstName: "Augusta", LastName: "Lovelace", Email: "ada@example.com"}, true},
		"one word (K3)":             {full, map[string]string{"n": "Ada", "e": "ada@example.com"}, ticket.GuestInfo{}, false},
		"not an address":            {full, map[string]string{"n": "Ada Lovelace", "e": "ada at example"}, ticket.GuestInfo{}, false},
		"an address without a dot":  {full, map[string]string{"n": "Ada Lovelace", "e": "ada@localhost"}, ticket.GuestInfo{}, false},
		"a named address":           {full, map[string]string{"n": "Ada Lovelace", "e": "Ada <ada@example.com>"}, ticket.GuestInfo{}, false},
		"no last name":              {split, map[string]string{"f": "Ada", "l": " ", "e": "ada@example.com"}, ticket.GuestInfo{}, false},
		"no answer":                 {split, map[string]string{}, ticket.GuestInfo{}, false},
		"a name too long":           {split, map[string]string{"f": strings.Repeat("a", 101), "l": "L", "e": "ada@example.com"}, ticket.GuestInfo{}, false},
		"a control character":       {split, map[string]string{"f": "Ada\x00", "l": "L", "e": "ada@example.com"}, ticket.GuestInfo{}, false},
	} {
		got, ok := MappedGuest(tc.m, tc.answers)
		if ok != tc.ok || got != tc.want {
			t.Errorf("%s: %+v %t", name, got, ok)
		}
	}
}

func TestResolveRespondentsPriority(t *testing.T) {
	user := uuid.New()
	evA, evB, evC := uuid.New(), uuid.New(), uuid.New()
	mapA := Mapping{EventID: evA, FullName: "n", Email: "e"}
	mapB := Mapping{EventID: evB, FullName: "n", Email: "e"}
	mapC := Mapping{EventID: evC, FullName: "other", Email: "e"}
	answers := map[string]string{"n": "Ada Lovelace", "e": "ada@example.com", "other": "Grace Hopper"}

	rs, out := ResolveRespondents(&user, &Guest{FirstName: "G"}, answers, []Mapping{mapA})
	if out != "" || len(rs) != 1 || rs[0].Source != RespondentUser || rs[0].Guest != nil {
		t.Fatalf("user: %+v %s", rs, out)
	}
	rs, _ = ResolveRespondents(nil, &Guest{FirstName: "G", LastName: "H", Email: "g@example.com"}, answers, []Mapping{mapA})
	if len(rs) != 1 || rs[0].Source != RespondentFormsGuest || rs[0].Guest.FirstName != "G" || rs[0].Events != nil {
		t.Fatalf("Forms' guest: %+v", rs)
	}
	rs, out = ResolveRespondents(nil, nil, answers, nil)
	if out != "" || len(rs) != 1 || rs[0].Source != RespondentNobody {
		t.Fatalf("nobody: %+v %s", rs, out)
	}
	// Two Events whose mappings give the same guest are one respondent;
	// another giving another guest is a second.
	rs, out = ResolveRespondents(nil, nil, answers, []Mapping{mapA, mapB, mapC})
	if out != "" || len(rs) != 2 || !slices.Equal(rs[0].Events, []uuid.UUID{evA, evB}) || rs[1].Guest.LastName != "Hopper" || !slices.Equal(rs[1].Events, []uuid.UUID{evC}) {
		t.Fatalf("mapped: %+v", rs)
	}
	if rs, out = ResolveRespondents(nil, nil, map[string]string{"n": "Ada", "e": "ada@example.com"}, []Mapping{mapA}); rs != nil || out != ticket.FormResponseMappingIncomplete {
		t.Fatalf("incomplete: %+v %s", rs, out)
	}
}

func TestRecordRespondentsAddsUp(t *testing.T) {
	var reports []ticket.FormResponse
	results := []ticket.FormResponseResult{
		{Outcome: ticket.FormResponseEventFull, ListingEvents: 2},
		{Outcome: ticket.FormResponseRecorded, TicketsWritten: 1, ListingEvents: 2},
	}
	record := func(_ context.Context, r ticket.FormResponse) (ticket.FormResponseResult, error) {
		reports = append(reports, r)
		return results[len(reports)-1], nil
	}
	evA, evB := uuid.New(), uuid.New()
	ada := ticket.GuestInfo{FirstName: "Ada", LastName: "Lovelace", Email: "ada@example.com"}
	grace := ticket.GuestInfo{FirstName: "Grace", LastName: "Hopper", Email: "grace@example.com"}
	got, err := RecordRespondents(context.Background(), record, ticket.FormResponse{FormID: applicationForm, ResponseID: responseID, Status: ticket.FormResponseAccepted},
		[]Respondent{{Source: RespondentMapped, Guest: &ada, Events: []uuid.UUID{evA}}, {Source: RespondentMapped, Guest: &grace, Events: []uuid.UUID{evB}}})
	if err != nil || got.Outcome != ticket.FormResponseMappedGuest || got.TicketsWritten != 1 || got.ListingEvents != 2 {
		t.Fatalf("%+v %v", got, err)
	}
	if len(reports) != 2 || *reports[0].Guest != ada || !slices.Equal(reports[0].GuestEvents, []uuid.UUID{evA}) || reports[1].ResponseID != responseID {
		t.Fatalf("%+v", reports)
	}
	// A user is one call as before: no Event limit.
	reports, results = nil, []ticket.FormResponseResult{{Outcome: ticket.FormResponseRecorded}}
	user := uuid.New()
	got, _ = RecordRespondents(context.Background(), record, ticket.FormResponse{}, []Respondent{{Source: RespondentUser, UserID: &user}})
	if got.Outcome != ticket.FormResponseRecorded || reports[0].GuestEvents != nil || *reports[0].UserID != user {
		t.Fatalf("%+v %+v", got, reports)
	}
}

type memMappingStore struct{ rows []Mapping }

func (m memMappingStore) ForForm(_ context.Context, formID uuid.UUID) ([]Mapping, error) {
	var out []Mapping
	for _, r := range m.rows {
		if r.FormID == formID {
			out = append(out, r)
		}
	}
	return out, nil
}

type fieldsReader struct {
	got    Response
	calls  int
	fields [][]string
}

func (f *fieldsReader) Response(_ context.Context, formID, responseID uuid.UUID, fields ...string) (Response, error) {
	f.calls++
	f.fields = append(f.fields, fields)
	got := f.got
	got.FormID, got.ResponseID = formID, responseID
	return got, nil
}

// mappedHarness is the harness with a mapping of the application form's
// Event, and a form another Event let go whose mapping is left.
func mappedHarness(answers map[string]string) (*harness, *fieldsReader, uuid.UUID) {
	h := newHarness()
	evs := h.rec.Events.(fakeEvents)
	mapped := evs.events[0].ID
	reader := &fieldsReader{got: Response{Status: StatusAccepted, Version: 1, Answers: answers}}
	h.rec.Forms = reader
	h.rec.Mapped = &MappedGuests{
		Store: memMappingStore{rows: []Mapping{
			{EventID: mapped, FormID: applicationForm, FullName: "n", Email: "e"},
			{EventID: uuid.New(), FormID: applicationForm, FirstName: "x", LastName: "y", Email: "z"},
		}},
		Events: evs,
	}
	return h, reader, mapped
}

// A core.forms event of an answer that names nobody: the single read asks
// for the mapped questions only, and the guest goes to the Ticket rule
// limited to the mapped Event.
func TestAnAnonymousEventTakesTheMapping(t *testing.T) {
	h, reader, mapped := mappedHarness(map[string]string{"n": "Ada Lovelace", "e": "ADA@example.com"})
	if err := h.handle(t, cloudEvent(TypeResponseCreated, created(applicationForm, StatusAccepted, 3, map[string]any{"answers": map[string]any{"n": "Eve Forged", "e": "eve@example.com"}}))); err != nil {
		t.Fatal(err)
	}
	if reader.calls != 1 || !slices.Equal(reader.fields[0], []string{"n", "e"}) {
		t.Fatalf("reads %d fields %v", reader.calls, reader.fields)
	}
	if len(h.reports) != 1 || h.reports[0].Guest == nil || h.reports[0].Guest.Email != "ada@example.com" || h.reports[0].Guest.FirstName != "Ada" ||
		!slices.Equal(h.reports[0].GuestEvents, []uuid.UUID{mapped}) || h.reports[0].UserID != nil {
		t.Fatalf("reports %+v", h.reports)
	}
	log := strings.Join(h.lines, "\n")
	if !strings.Contains(log, `"outcome":"mapped_guest"`) || strings.Contains(log, "ada@") || strings.Contains(log, "Lovelace") {
		t.Fatalf("log %s", log)
	}
}

func TestAnAnonymousAnswerWithoutAGuestInItsAnswers(t *testing.T) {
	h, _, _ := mappedHarness(map[string]string{"n": "Ada", "e": "ada@example.com"})
	if err := h.handle(t, cloudEvent(TypeResponseCreated, created(applicationForm, StatusAccepted, 3, nil))); err != nil {
		t.Fatal(err)
	}
	if len(h.reports) != 0 || !strings.Contains(strings.Join(h.lines, "\n"), `"outcome":"mapping_incomplete"`) {
		t.Fatalf("reports %+v log %v", h.reports, h.lines)
	}
}

// Without a mapping, or for a person or Forms' named guest, nothing changes.
func TestTheMappingOnlyForAnAnswerThatNamesNobody(t *testing.T) {
	h := newHarness()
	h.rec.Mapped = &MappedGuests{Store: memMappingStore{}, Events: h.rec.Events}
	if err := h.handle(t, cloudEvent(TypeResponseCreated, created(applicationForm, StatusAccepted, 3, nil))); err != nil {
		t.Fatal(err)
	}
	if len(h.reports) != 1 || h.reports[0].Guest != nil || h.reports[0].GuestEvents != nil || h.reader.calls != 0 {
		t.Fatalf("no mapping: %+v reads %d", h.reports, h.reader.calls)
	}
	h, reader, _ := mappedHarness(map[string]string{"n": "Ada Lovelace", "e": "ada@example.com"})
	if err := h.handle(t, cloudEvent(TypeResponseCreated, created(applicationForm, StatusAccepted, 3, map[string]any{"userId": personID}))); err != nil {
		t.Fatal(err)
	}
	if len(h.reports) != 1 || h.reports[0].UserID == nil || h.reports[0].Guest != nil || reader.calls != 0 {
		t.Fatalf("a person: %+v reads %d", h.reports, reader.calls)
	}
	// A declined anonymous answer goes as it came: the rule cancels what it
	// wrote (tickets.form_response_id).
	if err := h.handle(t, cloudEvent(TypeResponseStatusChanged, map[string]any{"formId": applicationForm, "responseId": responseID, "status": StatusDeclined, "userId": nil, "version": 9})); err != nil {
		t.Fatal(err)
	}
	if len(h.reports) != 2 || h.reports[1].Status != ticket.FormResponseDeclined || h.reports[1].Guest != nil || reader.calls != 0 {
		t.Fatalf("declined: %+v reads %d", h.reports, reader.calls)
	}
}

// A pull asks each page for the mapped questions and needs no single read.
func TestAPullAsksForTheMappedQuestions(t *testing.T) {
	h, reader, mapped := mappedHarness(nil)
	anon := Response{FormID: applicationForm, ResponseID: uuid.New(), Status: StatusAccepted, Version: 1, Answers: map[string]string{"n": "Ada Lovelace", "e": "ada@example.com"}}
	l := &pageLister{pages: map[string]Page{"": {Items: []Response{anon}}}}
	q := &memQueue{jobs: []PullJob{{ID: uuid.New(), FormID: applicationForm, Kind: PullCatchup, RequestedAt: pullNow}}}
	if _, err := newPuller(h, q, l).RunOne(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(l.queries[0].Fields, []string{"n", "e"}) || reader.calls != 0 {
		t.Fatalf("fields %v reads %d", l.queries[0].Fields, reader.calls)
	}
	if len(h.reports) != 1 || h.reports[0].Guest == nil || !slices.Equal(h.reports[0].GuestEvents, []uuid.UUID{mapped}) {
		t.Fatalf("reports %+v", h.reports)
	}
	if !strings.Contains(strings.Join(h.lines, "\n"), `"tickets_written":1`) {
		t.Fatalf("log %v", h.lines)
	}
}

// The mappings read are those of the Events listing the form now.
func TestMappingsOfTheListingEventsOnly(t *testing.T) {
	form := uuid.New()
	listing := event.Event{ID: uuid.New(), FormURL: "https://forms.example/" + form.String()}
	archivedAt := pullNow
	archived := event.Event{ID: uuid.New(), FormURL: "https://forms.example/" + form.String(), ArchivedAt: &archivedAt}
	letGo := event.Event{ID: uuid.New()}
	m := &MappedGuests{
		Store: memMappingStore{rows: []Mapping{
			{EventID: listing.ID, FormID: form, FullName: "a", Email: "b"},
			{EventID: archived.ID, FormID: form, FullName: "c", Email: "d"},
			{EventID: letGo.ID, FormID: form, FullName: "e", Email: "f"},
		}},
		Events: fakeEvents{events: []event.Event{listing, archived, letGo}},
	}
	got, err := m.Mappings(context.Background(), form)
	if err != nil || len(got) != 1 || got[0].EventID != listing.ID || !slices.Equal(MappedFields(got), []string{"a", "b"}) {
		t.Fatalf("%+v %v", got, err)
	}
	var none *MappedGuests
	if got, err := none.Mappings(context.Background(), form); got != nil || err != nil {
		t.Fatalf("nil: %v %v", got, err)
	}
}
