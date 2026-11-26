package formsfeed

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
)

func TestGradeOf(t *testing.T) {
	for answer, want := range map[string]string{
		"":                       "",
		"   ":                    "",
		"Hazırlık":               "prep",
		"HAZIRLIK SINIFI":        "prep",
		"Prep":                   "prep",
		"1":                      "1",
		"1. Sınıf":               "1",
		"2.sınıf":                "2",
		"3. sınıf öğrencisiyim":  "3",
		"4":                      "4",
		"Dördüncü sınıf":         "4",
		"İkinci":                 "2",
		"Third year":             "3",
		"Yüksek Lisans":          "masters",
		"YÜKSEK LİSANS":          "masters",
		"Yüksek lisans 1. sınıf": "masters",
		"Master's":               "masters",
		"Tezli YL":               "masters",
		"Doktora":                "phd",
		"PhD":                    "phd",
		"Ph.D.":                  "phd",
		"Mezun":                  "graduate",
		"Graduate":               "graduate",
		"Undergraduate":          "other",
		"5. sınıf":               "other",
		"6":                      "other",
		"Lisans":                 "other",
		"Lise":                   "other",
		"1. Sınıf, 2. Sınıf":     "other",
		"Hazırlık, 1. Sınıf":     "other",
		"Yüksek lisans, Doktora": "other",
		"bilmiyorum":             "other",
		"2024":                   "other",
	} {
		if got := GradeOf(answer); got != want {
			t.Errorf("GradeOf(%q) = %q, want %q", answer, got, want)
		}
		if got := GradeOf(answer); got != "" && !slices.Contains(ticket.Grades, got) {
			t.Errorf("GradeOf(%q) = %q is not a grade key", answer, got)
		}
	}
}

func TestMappedDetails(t *testing.T) {
	m := Mapping{Phone: "p", University: "u", Department: "d", Grade: "g"}
	got := MappedDetails(m, map[string]string{
		"p": " 0555 111 22 33 ", "u": "  Yıldız   Teknik Üniversitesi ", "d": "Bilgisayar\tMühendisliği", "g": "3. Sınıf",
	})
	if got.Phone != "+905551112233" || got.University != "Yıldız Teknik Üniversitesi" || got.Department != "Bilgisayar Mühendisliği" ||
		got.Grade != "3" || len(got.Dropped) != 0 {
		t.Fatalf("details %+v", got)
	}
	// A detail that does not read is left out, and named; the rest stay.
	got = MappedDetails(m, map[string]string{"p": "yok", "u": strings.Repeat("ü", 201), "d": "Fizik", "g": "Lise"})
	if got.Phone != "" || got.University != "" || got.Department != "Fizik" || got.Grade != "other" ||
		!slices.Equal(got.Dropped, []string{DetailPhone, DetailUniversity}) {
		t.Fatalf("details %+v", got)
	}
	// No question, or no answer: nothing, nothing dropped.
	if got := MappedDetails(Mapping{}, map[string]string{"p": "05551112233"}); got.Phone != "" || got.Dropped != nil {
		t.Fatalf("no question %+v", got)
	}
	if got := MappedDetails(m, nil); got.Phone != "" || got.University != "" || got.Department != "" || got.Grade != "" || got.Dropped != nil {
		t.Fatalf("no answers %+v", got)
	}
}

func TestMappingRuleWithDetails(t *testing.T) {
	m := Mapping{FullName: "n", Email: "e", Phone: "p", University: "u", Department: "d", Grade: "g"}
	if err := m.Validate(); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(m.Fields(), []string{"n", "e", "p", "u", "d", "g"}) || !slices.Equal(m.DetailFields(), []string{"p", "u", "d", "g"}) || !m.HasDetails() {
		t.Fatalf("fields %v %v", m.Fields(), m.DetailFields())
	}
	for name, bad := range map[string]Mapping{
		"details without the guest": {Phone: "p"},
		"a detail twice":            {FullName: "n", Email: "e", Phone: "x", Grade: "x"},
		"a detail as the e-mail":    {FullName: "n", Email: "e", University: "e"},
		"a comma":                   {FullName: "n", Email: "e", Department: "a,b"},
		"too long":                  {FullName: "n", Email: "e", Grade: strings.Repeat("g", MaxFieldLength+1)},
	} {
		if err := bad.Validate(); err == nil {
			t.Errorf("%s: valid", name)
		}
	}
	if (Mapping{FullName: "n", Email: "e"}).HasDetails() {
		t.Fatal("no details: HasDetails")
	}
	got := Mapping{FullName: " n ", Email: "e", Phone: " p ", Grade: " g "}.Normalized()
	if got.Phone != "p" || got.Grade != "g" {
		t.Fatalf("normalized %+v", got)
	}
}

// ResolveRespondents: a mapped guest and Forms' named guest take the
// details; a signed-in person none.
func TestResolveRespondentsTakesTheDetails(t *testing.T) {
	event := uuid.New()
	mappings := []Mapping{{EventID: event, FullName: "n", Email: "e", Phone: "p", Grade: "g"}}
	answers := map[string]string{"n": "Ada Lovelace", "e": "ada@example.com", "p": "+44 20 7946 0958", "g": "Doktora"}
	got, outcome := ResolveRespondents(nil, nil, answers, mappings)
	if outcome != "" || len(got) != 1 || got[0].Guest.PhoneNumber != "+442079460958" || got[0].Guest.Grade != "phd" || got[0].Guest.University != "" {
		t.Fatalf("mapped %+v %q", got, outcome)
	}
	got, _ = ResolveRespondents(nil, &Guest{FirstName: "Ada", LastName: "King", Email: "ada@example.com"}, map[string]string{"p": "05551112233", "g": "4. sınıf"}, mappings)
	if len(got) != 1 || got[0].Source != RespondentFormsGuest || got[0].Events != nil ||
		*got[0].Guest != (ticket.GuestInfo{FirstName: "Ada", LastName: "King", Email: "ada@example.com", PhoneNumber: "+905551112233", Grade: "4"}) {
		t.Fatalf("named %+v", got)
	}
	user := uuid.New()
	got, _ = ResolveRespondents(&user, nil, answers, mappings)
	if len(got) != 1 || got[0].Guest != nil {
		t.Fatalf("person %+v", got)
	}
	// Two Events' mappings: each detail from the first that answers it.
	two := append(mappings, Mapping{EventID: uuid.New(), FullName: "n", Email: "e", University: "u", Grade: "g2"})
	got, _ = ResolveRespondents(nil, &Guest{FirstName: "Ada", LastName: "King", Email: "ada@example.com"}, map[string]string{"g": "", "g2": "Mezun", "u": "YTÜ"}, two)
	if g := got[0].Guest; g.Grade != "graduate" || g.University != "YTÜ" {
		t.Fatalf("two mappings %+v", g)
	}
}

// A core.forms event of Forms' named guest: the single read asks for the
// mappings' details alone (the name and e-mail are Forms'), and the
// guest's report carries them; a mapping without details asks for none.
func TestANamedGuestTakesTheMappingsDetails(t *testing.T) {
	h := newHarness()
	evs := h.rec.Events.(fakeEvents)
	reader := &fieldsReader{got: Response{Status: StatusAccepted, Version: 3, HasGuest: true,
		Guest:   &Guest{FirstName: "Ada", LastName: "Lovelace", Email: "ada@example.com"},
		Answers: map[string]string{"p": "0555 111 2233", "u": "YTÜ", "d": "Fizik", "g": "Hazırlık"}}}
	h.rec.Forms = reader
	h.rec.Mapped = &MappedGuests{Store: memMappingStore{rows: []Mapping{
		{EventID: evs.events[0].ID, FormID: applicationForm, FullName: "n", Email: "e", Phone: "p", University: "u", Department: "d", Grade: "g"},
	}}, Events: evs}
	if err := h.handle(t, cloudEvent(TypeResponseCreated, created(applicationForm, StatusAccepted, 3, map[string]any{"hasGuest": true}))); err != nil {
		t.Fatal(err)
	}
	if reader.calls != 1 || !slices.Equal(reader.fields[0], []string{"p", "u", "d", "g"}) {
		t.Fatalf("reads %d fields %v", reader.calls, reader.fields)
	}
	want := ticket.GuestInfo{FirstName: "Ada", LastName: "Lovelace", Email: "ada@example.com",
		PhoneNumber: "+905551112233", University: "YTÜ", Department: "Fizik", Grade: "prep"}
	if len(h.reports) != 1 || h.reports[0].Guest == nil || *h.reports[0].Guest != want || h.reports[0].GuestEvents != nil {
		t.Fatalf("reports %+v", h.reports)
	}
	for _, line := range h.lines {
		if strings.Contains(line, "1112233") || strings.Contains(line, "Fizik") || strings.Contains(line, "YTÜ") {
			t.Fatalf("a log line names a detail: %s", line)
		}
	}

	h, reader, _ = mappedHarness(nil)
	reader.got = Response{Status: StatusAccepted, Version: 3, HasGuest: true, Guest: &Guest{FirstName: "Ada", LastName: "Lovelace", Email: "ada@example.com"}}
	if err := h.handle(t, cloudEvent(TypeResponseCreated, created(applicationForm, StatusAccepted, 3, map[string]any{"hasGuest": true}))); err != nil {
		t.Fatal(err)
	}
	if reader.calls != 1 || len(reader.fields[0]) != 0 {
		t.Fatalf("no details: reads %d fields %v", reader.calls, reader.fields)
	}
	if len(h.reports) != 1 || *h.reports[0].Guest != (ticket.GuestInfo{FirstName: "Ada", LastName: "Lovelace", Email: "ada@example.com"}) {
		t.Fatalf("no details: %+v", h.reports)
	}
}

// The HTTP report of Forms' named guest reads the details when a mapping
// names any; without them it goes to the rule as it came, unread.
func TestTheHTTPReportOfANamedGuestReadsTheDetails(t *testing.T) {
	evs := newHarness().rec.Events.(fakeEvents)
	reader := &fieldsReader{got: Response{Status: StatusAccepted, Version: 1, Answers: map[string]string{"d": "Matematik", "g": "2"}}}
	var reports []ticket.FormResponse
	record := func(_ context.Context, r ticket.FormResponse) (ticket.FormResponseResult, error) {
		reports = append(reports, r)
		return ticket.FormResponseResult{Outcome: ticket.FormResponseRecorded}, nil
	}
	named := ticket.FormResponse{FormID: applicationForm, ResponseID: responseID, Status: ticket.FormResponseAccepted,
		Guest: &ticket.GuestInfo{FirstName: "Ada", LastName: "Lovelace", Email: "ada@example.com"}}
	m := &MappedGuests{Store: memMappingStore{rows: []Mapping{{EventID: evs.events[0].ID, FormID: applicationForm, FullName: "n", Email: "e", Department: "d", Grade: "g"}}},
		Events: evs, Forms: reader}
	if _, err := m.RecordReport(context.Background(), record, named); err != nil {
		t.Fatal(err)
	}
	if reader.calls != 1 || !slices.Equal(reader.fields[0], []string{"d", "g"}) || len(reports) != 1 ||
		reports[0].Guest.Department != "Matematik" || reports[0].Guest.Grade != "2" || reports[0].Guest.FirstName != "Ada" || reports[0].GuestEvents != nil {
		t.Fatalf("reads %d %v reports %+v", reader.calls, reader.fields, reports)
	}
	m.Store = memMappingStore{rows: []Mapping{{EventID: evs.events[0].ID, FormID: applicationForm, FullName: "n", Email: "e"}}}
	reports = nil
	if _, err := m.RecordReport(context.Background(), record, named); err != nil {
		t.Fatal(err)
	}
	if reader.calls != 1 || len(reports) != 1 || reports[0].Guest != named.Guest {
		t.Fatalf("no details: reads %d reports %+v", reader.calls, reports)
	}
}

// A respondent row keeps a guest's details, and its Ticket takes them.
func TestARespondentRowKeepsTheDetails(t *testing.T) {
	g := ticket.GuestInfo{FirstName: "Ada", LastName: "Lovelace", Email: "ada@example.com", PhoneNumber: "+905551112233", University: "YTÜ", Department: "Fizik", Grade: "1"}
	row, ok := respondentRow(Respondent{Source: RespondentMapped, Guest: &g}, uuid.New(), applicationForm, responseID, nil, StatusAccepted, SourceCollect)
	if !ok || row.Phone != g.PhoneNumber || row.University != "YTÜ" || row.Department != "Fizik" || row.Grade != "1" {
		t.Fatalf("row %+v", row)
	}
	if back := rowRespondent(row); back.Guest == nil || *back.Guest != g {
		t.Fatalf("back %+v", back)
	}
	user := uuid.New()
	row, _ = respondentRow(Respondent{Source: RespondentUser, UserID: &user}, uuid.New(), applicationForm, responseID, nil, StatusAccepted, SourceCollect)
	if row.Phone != "" || row.Grade != "" {
		t.Fatalf("person row %+v", row)
	}
}
