package formsfeed_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/formsfeed"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
)

// detailAnswers is Forms' single read of an answer: the answers of each
// response id, those asked for only.
type detailAnswers map[uuid.UUID]map[string]string

func (a detailAnswers) Response(_ context.Context, formID, responseID uuid.UUID, fields ...string) (formsfeed.Response, error) {
	asked := map[string]string{}
	for _, f := range fields {
		if v, ok := a[responseID][f]; ok {
			asked[f] = v
		}
	}
	return formsfeed.Response{FormID: formID, ResponseID: responseID, Status: formsfeed.StatusAccepted, Version: 1, Answers: asked}, nil
}

// detailsLister is Forms' list, recording what core asked for.
type detailsLister struct {
	pages  map[string]formsfeed.Page
	fields [][]string
}

func (l *detailsLister) List(_ context.Context, _ uuid.UUID, q formsfeed.ListQuery) (formsfeed.Page, error) {
	if !q.AllStatuses {
		return formsfeed.Page{}, errors.New("asked for accepted answers only")
	}
	l.fields = append(l.fields, q.Fields)
	return l.pages[q.After], nil
}

func (w respondentWorld) guestDetails(t *testing.T, table, where string, args ...any) [4]string {
	t.Helper()
	cols := "guest_phone_number, guest_university, guest_department, guest_grade"
	if table == "event_form_respondents" {
		cols = "phone_number, university, department, grade"
	}
	var got [4]string
	if err := w.pool.QueryRow(context.Background(), `SELECT `+cols+` FROM `+table+` WHERE `+where, args...).Scan(&got[0], &got[1], &got[2], &got[3]); err != nil {
		t.Fatal(err)
	}
	return got
}

func TestPostgresMappingsKeepTheDetails(t *testing.T) {
	w := newRespondentWorld(t, true, 0, true)
	ctx := context.Background()
	store := formsfeed.NewPostgresMappings(w.pool)
	saved, _, err := store.Put(ctx, formsfeed.Mapping{EventID: w.ev.ID, FormID: w.formID, FullName: "n", Email: "e", Phone: " p", University: "u", Department: "d", Grade: "g"})
	if err != nil || saved.Phone != "p" || saved.University != "u" || saved.Department != "d" || saved.Grade != "g" {
		t.Fatalf("put %+v %v", saved, err)
	}
	got, err := store.Get(ctx, w.ev.ID, w.formID)
	if err != nil || got.Phone != "p" || got.Grade != "g" {
		t.Fatalf("get %+v %v", got, err)
	}
	if saved, _, err = store.Put(ctx, formsfeed.Mapping{EventID: w.ev.ID, FormID: w.formID, FullName: "n", Email: "e"}); err != nil || saved.HasDetails() {
		t.Fatalf("replaced %+v %v", saved, err)
	}
	if _, err := w.pool.Exec(ctx, `UPDATE event_form_mappings SET grade_question = ' ' WHERE event_id = $1`, w.ev.ID); err == nil {
		t.Fatal("a blank question id was kept")
	}
}

// Forms' named guest with an Event whose "Otomatik bilet gönder" is off:
// the details go on the row, and the Ticket sent later takes them.
func TestTheDetailsGoThroughTheRowToTheTicket(t *testing.T) {
	w := newRespondentWorld(t, false, 0, true)
	ctx := context.Background()
	if _, _, err := formsfeed.NewPostgresMappings(w.pool).Put(ctx, formsfeed.Mapping{
		EventID: w.ev.ID, FormID: w.formID, FullName: "n", Email: "e", Phone: "p", University: "u", Department: "d", Grade: "g",
	}); err != nil {
		t.Fatal(err)
	}
	answer := uuid.New()
	w.reports.Forms = detailAnswers{answer: {"n": "Eve Forged", "e": "eve@example.com", "p": "0555 111 22 33", "u": "YTÜ", "d": "Fizik", "g": "2. Sınıf"}}
	w.report(t, answer, ticket.FormResponseAccepted, "ada@example.com")
	want := [4]string{"+905551112233", "YTÜ", "Fizik", "2"}
	if got := w.guestDetails(t, "event_form_respondents", "form_response_id = $1", answer); got != want {
		t.Fatalf("row %v", got)
	}
	rows := w.rows(t)
	if len(rows) != 1 || rows[0].Email != "ada@example.com" {
		t.Fatalf("rows %+v", rows)
	}
	if res, _, err := w.rs.Send(ctx, w.editor, w.ev.ID, formsfeed.Selection{IDs: []uuid.UUID{rows[0].ID}}); err != nil || res.Recorded != 1 {
		t.Fatalf("send %+v %v", res, err)
	}
	if got := w.guestDetails(t, "tickets", "form_response_id = $1", answer); got != want {
		t.Fatalf("ticket %v", got)
	}
}

// The fill of earlier guests' details: a dry run counts and writes nothing;
// the run fills only what a Ticket or a respondent row lacks; a run again
// finds nothing to do. Never a signed-in person's Ticket.
func TestFillingInEarlierGuestsDetails(t *testing.T) {
	w := newRespondentWorld(t, true, 0, true)
	ctx := context.Background()
	store := formsfeed.NewPostgresMappings(w.pool)
	if _, _, err := store.Put(ctx, formsfeed.Mapping{EventID: w.ev.ID, FormID: w.formID, FullName: "n", Email: "e"}); err != nil {
		t.Fatal(err)
	}
	ada, grace, linus, ken := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	w.report(t, ada, ticket.FormResponseAccepted, "ada@example.com")
	w.report(t, grace, ticket.FormResponseAccepted, "grace@example.com")
	w.report(t, linus, ticket.FormResponseAccepted, "linus@example.com")
	w.report(t, ken, ticket.FormResponsePending, "ken@example.com")
	if _, err := w.pool.Exec(ctx, `UPDATE tickets SET guest_phone_number = '+905550000000' WHERE form_response_id = $1`, grace); err != nil {
		t.Fatal(err)
	}
	lister := &detailsLister{pages: map[string]formsfeed.Page{
		"": {NextCursor: "c1", Items: []formsfeed.Response{
			{FormID: w.formID, ResponseID: ada, Status: formsfeed.StatusAccepted, Answers: map[string]string{"p": "0555 111 22 33", "u": "YTÜ", "d": "Fizik", "g": "3. sınıf"}},
			{FormID: w.formID, ResponseID: grace, Status: formsfeed.StatusAccepted, Answers: map[string]string{"p": "+905559999999", "g": "Doktora"}},
		}},
		"c1": {Items: []formsfeed.Response{
			{FormID: w.formID, ResponseID: linus, Status: formsfeed.StatusAccepted, Answers: map[string]string{"p": "yok", "u": strings.Repeat("ü", 201), "g": "Lise"}},
			{FormID: w.formID, ResponseID: ken, Status: formsfeed.StatusPending, Answers: map[string]string{"u": "İTÜ", "g": "Hazırlık"}},
			{FormID: w.formID, ResponseID: uuid.New(), Status: formsfeed.StatusAccepted, Answers: map[string]string{"g": "1"}},
		}},
	}}
	fill := &formsfeed.DetailsBackfill{Pool: w.pool, Events: w.events, Forms: lister}
	if _, err := fill.FillDetails(ctx, w.ev.ID, false); !errors.Is(err, formsfeed.ErrDetailsNoMapping) {
		t.Fatalf("no details' questions: %v", err)
	}
	if _, err := fill.FillDetails(ctx, uuid.New(), false); !errors.Is(err, formsfeed.ErrDetailsEventNotFound) {
		t.Fatalf("no Event: %v", err)
	}
	if _, _, err := store.Put(ctx, formsfeed.Mapping{EventID: w.ev.ID, FormID: w.formID, FullName: "n", Email: "e", Phone: "p", University: "u", Department: "d", Grade: "g"}); err != nil {
		t.Fatal(err)
	}

	dry, err := fill.FillDetails(ctx, w.ev.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if dry.Forms != 1 || dry.Answers != 5 || dry.Tickets != 3 || dry.TicketsFilled != 3 || dry.Respondents != 1 || dry.RespondentsFilled != 1 {
		t.Fatalf("dry run %+v", dry)
	}
	if dry.Filled[formsfeed.DetailPhone] != 1 || dry.Filled[formsfeed.DetailUniversity] != 1 || dry.Filled[formsfeed.DetailDepartment] != 1 ||
		dry.Filled[formsfeed.DetailGrade] != 3 || dry.Kept[formsfeed.DetailPhone] != 1 ||
		dry.Dropped[formsfeed.DetailPhone] != 1 || dry.Dropped[formsfeed.DetailUniversity] != 1 ||
		dry.Grades["3"] != 1 || dry.Grades["phd"] != 1 || dry.Grades["other"] != 1 || dry.Grades["prep"] != 1 || dry.Grades["1"] != 1 {
		t.Fatalf("dry run counts %+v", dry)
	}
	if !slices.Equal(lister.fields[0], []string{"p", "u", "d", "g"}) {
		t.Fatalf("asked %v", lister.fields)
	}
	if got := w.guestDetails(t, "tickets", "form_response_id = $1", ada); got != ([4]string{}) {
		t.Fatalf("the dry run wrote %v", got)
	}

	if _, err := fill.FillDetails(ctx, w.ev.ID, true); err != nil {
		t.Fatal(err)
	}
	for id, want := range map[uuid.UUID][4]string{
		ada:   {"+905551112233", "YTÜ", "Fizik", "3"},
		grace: {"+905550000000", "", "", "phd"},
		linus: {"", "", "", "other"},
	} {
		if got := w.guestDetails(t, "tickets", "form_response_id = $1", id); got != want {
			t.Errorf("ticket %v, want %v", got, want)
		}
	}
	if got := w.guestDetails(t, "event_form_respondents", "form_response_id = $1", ken); got != ([4]string{"", "İTÜ", "", "prep"}) {
		t.Fatalf("row %v", got)
	}
	again, err := fill.FillDetails(ctx, w.ev.ID, false)
	if err != nil || again.TicketsFilled != 0 || again.RespondentsFilled != 0 || again.Tickets != 3 {
		t.Fatalf("again %+v %v", again, err)
	}
}
