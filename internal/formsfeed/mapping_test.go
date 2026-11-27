package formsfeed

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/event"
)

func TestMappingRule(t *testing.T) {
	for name, tc := range map[string]struct {
		m  Mapping
		ok bool
	}{
		"first, last, e-mail":    {Mapping{FirstName: "a", LastName: "b", Email: "c"}, true},
		"full name, e-mail":      {Mapping{FullName: "a", Email: "c"}, true},
		"first and full, e-mail": {Mapping{FirstName: "a", FullName: "b", Email: "c"}, true},
		"all four":               {Mapping{FirstName: "a", LastName: "b", FullName: "d", Email: "c"}, true},
		"padded ids":             {Mapping{FullName: " a ", Email: " c"}, true},
		"no e-mail":              {Mapping{FirstName: "a", LastName: "b"}, false},
		"first only":             {Mapping{FirstName: "a", Email: "c"}, false},
		"blank full name":        {Mapping{FullName: "  ", Email: "c"}, false},
		"one question twice":     {Mapping{FullName: "a", Email: "a"}, false},
		"an id with a comma":     {Mapping{FullName: "a,b", Email: "c"}, false},
		"an id over 100 bytes":   {Mapping{FullName: strings.Repeat("q", 101), Email: "c"}, false},
		"nothing":                {Mapping{}, false},
	} {
		if err := tc.m.Validate(); (err == nil) != tc.ok || (err != nil && !errors.Is(err, ErrMappingInvalid)) {
			t.Errorf("%s: %v", name, err)
		}
	}
	m := Mapping{FirstName: "a", LastName: "b", Email: "c"}
	if got := m.Fields(); !slices.Equal(got, []string{"a", "b", "c"}) {
		t.Fatalf("fields %v", got)
	}
	if got := m.MissingFrom([]Question{{ID: "a"}, {ID: "c"}}); !slices.Equal(got, []string{"b"}) {
		t.Fatalf("missing %v", got)
	}
}

// K5: a form's identity fields give the suggestion; one two questions claim
// is left empty, and nothing else is guessed here.
func TestSuggestFromIdentityFields(t *testing.T) {
	got := Suggest([]Question{
		{ID: "q1", Identity: "firstName"}, {ID: "q2", Identity: "lastName"}, {ID: "q3", Identity: "email"},
		{ID: "q4", InputType: "email", Question: "E-posta"},
	})
	if got != (Mapping{FirstName: "q1", LastName: "q2", Email: "q3"}) {
		t.Fatalf("%+v", got)
	}
	got = Suggest([]Question{{ID: "q1", Identity: "email"}, {ID: "q2", Identity: "email"}, {ID: "q3", Question: "Ad Soyad"}})
	if got != (Mapping{}) {
		t.Fatalf("%+v", got)
	}
}

func TestClientReadsAFormsQuestions(t *testing.T) {
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		w.Write([]byte(`{"formId":"` + applicationForm.String() + `","title":"Başvuru","questions":[
			{"id":"q1","type":"short_text","question":"Adınız","inputType":"name","identity":null},
			{"id":"q2","type":"short_text","question":"E-posta","inputType":"email","identity":"email"},
			{"id":"","type":"heading","question":"Başlık","inputType":null,"identity":null}],
			"collaborators":[{"userId":"` + personID.String() + `","role":"Owner"},{"userId":"` + responseID.String() + `","role":"viewer"}]}`))
	}))
	defer srv.Close()
	got, err := (&Client{BaseURL: srv.URL, Tokens: &fakeTokens{}}).Form(context.Background(), applicationForm)
	if err != nil {
		t.Fatal(err)
	}
	if path != "/internal/v1/forms/"+applicationForm.String() || got.Title != "Başvuru" || len(got.Questions) != 2 ||
		got.Questions[0] != (Question{ID: "q1", Type: "short_text", Question: "Adınız", InputType: "name"}) || got.Questions[1].Identity != "email" {
		t.Fatalf("%s %+v", path, got)
	}
	// The owner and a viewer are on the team; nobody else is (Yusuf,
	// 2026-11-26: a viewer too).
	if !got.AccessListed || !got.Sees(personID.String()) || !got.Sees(responseID.String()) || got.Sees(uuid.NewString()) || got.Sees("not-a-uuid") {
		t.Fatalf("team %+v", got.Collaborators)
	}
	for role, sees := range map[string]bool{RoleOwner: true, RoleEditor: true, RoleViewer: true, "commenter": false, "": false} {
		f := Form{AccessListed: true, Collaborators: []Collaborator{{UserID: personID, Role: role}}}
		if f.Sees(personID.String()) != sees {
			t.Fatalf("role %q sees %t", role, !sees)
		}
	}
	if _, err := (&Client{BaseURL: srv.URL, Tokens: &fakeTokens{}}).Form(context.Background(), uuid.New()); !errors.Is(err, ErrInvalid) {
		t.Fatalf("another form: %v", err)
	}
	// A Forms before the team field lets nobody in.
	old := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"formId":"` + applicationForm.String() + `","title":"Eski","questions":[]}`))
	}))
	defer old.Close()
	got, err = (&Client{BaseURL: old.URL, Tokens: &fakeTokens{}}).Form(context.Background(), applicationForm)
	if err != nil || got.AccessListed || got.Sees(personID.String()) {
		t.Fatalf("no team field: %+v %v", got, err)
	}
}

// fields goes as one sorted value, each id once; answers come back by id,
// a value that is not a string left out.
func TestClientAsksForTheMappedAnswers(t *testing.T) {
	var queries []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		queries = append(queries, r.URL.Query().Get("fields"))
		item := `{"formId":"` + applicationForm.String() + `","responseId":"` + responseID.String() + `","status":"accepted","userId":null,
			"hasGuest":false,"submittedAt":"2026-11-20T09:15:00Z","version":"2026-11-20T09:16:00Z","answers":{"q2":"ada@example.com","q1":"Ada Lovelace","q9":7}}`
		if strings.HasSuffix(r.URL.Path, "/responses") {
			w.Write([]byte(`{"items":[` + item + `],"nextCursor":null,"watermark":"2026-11-25T10:00:00Z"}`))
			return
		}
		w.Write([]byte(item))
	}))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL, Tokens: &fakeTokens{}}
	got, err := c.Response(context.Background(), applicationForm, responseID, "q2", "q1", "q2")
	if err != nil {
		t.Fatal(err)
	}
	if got.Answers["q1"] != "Ada Lovelace" || got.Answers["q2"] != "ada@example.com" || len(got.Answers) != 2 {
		t.Fatalf("%+v", got.Answers)
	}
	page, err := c.List(context.Background(), applicationForm, ListQuery{Fields: []string{"q2", "q1"}})
	if err != nil || len(page.Items) != 1 || page.Items[0].Answers["q1"] != "Ada Lovelace" {
		t.Fatalf("%+v %v", page, err)
	}
	if _, err := c.Response(context.Background(), applicationForm, responseID); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(queries, []string{"q1,q2", "q1,q2", ""}) {
		t.Fatalf("fields sent %q", queries)
	}
	for _, bad := range [][]string{{""}, {"a,b"}, {strings.Repeat("q", 101)}} {
		if _, err := c.Response(context.Background(), applicationForm, responseID, bad...); !errors.Is(err, ErrInvalid) {
			t.Fatalf("%q: %v", bad, err)
		}
	}
	many := make([]string, MaxFields+1)
	for i := range many {
		many[i] = uuid.NewString()
	}
	if _, err := c.List(context.Background(), applicationForm, ListQuery{Fields: many}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("21 fields: %v", err)
	}
	if len(queries) != 3 {
		t.Fatalf("a bad fields was sent: %q", queries)
	}
}

type memMappings struct {
	mu   sync.Mutex
	rows map[[2]uuid.UUID]Mapping
}

func (m *memMappings) Get(_ context.Context, eventID, formID uuid.UUID) (Mapping, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	got, ok := m.rows[[2]uuid.UUID{eventID, formID}]
	if !ok {
		return Mapping{}, ErrMappingNotFound
	}
	return got, nil
}

func (m *memMappings) Put(_ context.Context, in Mapping) (Mapping, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.rows == nil {
		m.rows = map[[2]uuid.UUID]Mapping{}
	}
	key := [2]uuid.UUID{in.EventID, in.FormID}
	_, had := m.rows[key]
	m.rows[key] = in
	return in, !had, nil
}

func (m *memMappings) Delete(_ context.Context, eventID, formID uuid.UUID) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := [2]uuid.UUID{eventID, formID}
	_, had := m.rows[key]
	delete(m.rows, key)
	return had, nil
}

func (m *memMappings) ForForm(_ context.Context, formID uuid.UUID) ([]Mapping, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Mapping
	for _, row := range m.rows {
		if row.FormID == formID {
			out = append(out, row)
		}
	}
	slices.SortFunc(out, func(a, b Mapping) int { return strings.Compare(a.EventID.String(), b.EventID.String()) })
	return out, nil
}

type stubForm struct {
	form Form
	err  error
}

func (s stubForm) Form(context.Context, uuid.UUID) (Form, error) { return s.form, s.err }

func TestMappingRoutesRules(t *testing.T) {
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	form := uuid.New()
	address := "https://forms.yildizskylab.com/" + form.String()
	open := event.Event{ID: uuid.New(), OwnerTeam: "WEBLAB", Status: event.StatusPublished, FormURL: address}
	draft := event.Event{ID: uuid.New(), OwnerTeam: "WEBLAB", Status: event.StatusDraft, FormURL: address}
	editor := authz.Principal{ID: uuid.NewString(), Groups: []string{"/UYELER/YK"}}
	member := authz.Principal{ID: uuid.NewString(), Groups: []string{"/UYELER/ARGE/GAMELAB"}}
	service := authz.Principal{ID: uuid.NewString(), ServiceAccount: true, Groups: []string{"/UYELER/YK"}}
	team := func(role string) []Collaborator {
		return []Collaborator{{UserID: uuid.MustParse(editor.ID), Role: role}, {UserID: uuid.New(), Role: RoleOwner}}
	}
	withTeam := func(collaborators []Collaborator, listed bool) Form {
		return Form{FormID: form, Title: "Başvuru", Questions: []Question{{ID: "q1", Identity: "email"}, {ID: "q2"}, {ID: "q3"}}, Collaborators: collaborators, AccessListed: listed}
	}
	questions := withTeam(team(RoleEditor), true)
	valid := Mapping{EventID: open.ID, FormID: form, FirstName: "q2", LastName: "q3", Email: "q1"}
	build := func(on bool, forms FormReader) (*Mappings, *memMappings) {
		store := &memMappings{}
		return &Mappings{
			Events: backfillEvents{events: []event.Event{open, draft}}, Authz: authz.NewAuthorizer(authz.DefaultPolicy()),
			Store: store, Forms: forms, On: on, Now: func() time.Time { return now },
		}, store
	}
	ctx := context.Background()

	m, store := build(true, stubForm{form: questions})
	got, err := m.Questions(ctx, editor, open.ID, form)
	if err != nil || len(got.Questions) != 3 || got.Suggestion.Email != "q1" {
		t.Fatalf("questions: %+v %v", got, err)
	}
	saved, created, err := m.Put(ctx, editor, valid)
	if err != nil || !created || saved.UpdatedBy == nil || saved.UpdatedBy.String() != editor.ID || !saved.UpdatedAt.Equal(now) {
		t.Fatalf("put: %+v %t %v", saved, created, err)
	}
	if _, created, err := m.Put(ctx, editor, valid); err != nil || created {
		t.Fatalf("second put: %t %v", created, err)
	}
	if got, err := m.Get(ctx, editor, open.ID, form); err != nil || got.Email != "q1" {
		t.Fatalf("get: %+v %v", got, err)
	}
	if err := m.Delete(ctx, editor, open.ID, form); err != nil || len(store.rows) != 0 {
		t.Fatalf("delete: %v", err)
	}
	if _, err := m.Get(ctx, editor, open.ID, form); !errors.Is(err, ErrMappingNotFound) {
		t.Fatalf("get after delete: %v", err)
	}
	if err := m.Delete(ctx, editor, open.ID, form); !errors.Is(err, ErrMappingNotFound) {
		t.Fatalf("delete again: %v", err)
	}

	cases := []struct {
		name  string
		p     authz.Principal
		ev    uuid.UUID
		form  uuid.UUID
		in    Mapping
		on    bool
		forms FormReader
		want  error
	}{
		{name: "may not edit", p: member, ev: open.ID, form: form, on: true, forms: stubForm{form: questions}, want: ErrMappingForbidden},
		{name: "a service account", p: service, ev: open.ID, form: form, on: true, forms: stubForm{form: questions}, want: ErrMappingForbidden},
		{name: "a draft hidden", p: member, ev: draft.ID, form: form, on: true, forms: stubForm{form: questions}, want: ErrMappingEventNotFound},
		{name: "no such Event", p: editor, ev: uuid.New(), form: form, on: true, forms: stubForm{form: questions}, want: ErrMappingEventNotFound},
		{name: "not listed", p: editor, ev: open.ID, form: uuid.New(), on: true, forms: stubForm{form: questions}, want: ErrMappingNotListed},
		{name: "breaks the rule", p: editor, ev: open.ID, form: form, in: Mapping{FullName: "q2"}, on: true, forms: stubForm{form: questions}, want: ErrMappingInvalid},
		{name: "a question the form lacks", p: editor, ev: open.ID, form: form, in: Mapping{FullName: "q9", Email: "q1"}, on: true, forms: stubForm{form: questions}, want: &MappingQuestionMissingError{}},
		{name: "no Forms client", p: editor, ev: open.ID, form: form, on: true, want: ErrFormsUnavailable},
		{name: "Forms down", p: editor, ev: open.ID, form: form, on: true, forms: stubForm{err: errors.New("formsfeed: Forms answered 503")}, want: ErrFormsUnavailable},
		{name: "the form gone", p: editor, ev: open.ID, form: form, on: true, forms: stubForm{err: ErrNotFound}, want: ErrFormGone},
		{name: "not on the form's team", p: editor, ev: open.ID, form: form, on: true, forms: stubForm{form: withTeam([]Collaborator{{UserID: uuid.New(), Role: RoleOwner}}, true)}, want: ErrFormsAccessDenied},
		{name: "a Forms without the team", p: editor, ev: open.ID, form: form, on: true, forms: stubForm{form: withTeam(nil, false)}, want: ErrFormsAccessDenied},
		{name: "off", p: editor, ev: open.ID, form: form, on: false, forms: stubForm{form: questions}, want: ErrFormsPullOff},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, store := build(tc.on, tc.forms)
			in := tc.in
			if in == (Mapping{}) {
				in = valid
			}
			in.EventID, in.FormID = tc.ev, tc.form
			_, _, err := m.Put(ctx, tc.p, in)
			var missing *MappingQuestionMissingError
			if _, isMissing := tc.want.(*MappingQuestionMissingError); isMissing {
				if !errors.As(err, &missing) || !slices.Equal(missing.Missing, []string{"q9"}) {
					t.Fatalf("put: %v", err)
				}
			} else if !errors.Is(err, tc.want) {
				t.Fatalf("put: %v, want %v", err, tc.want)
			}
			if len(store.rows) != 0 {
				t.Fatal("a refused put saved")
			}
		})
	}

	// Off: every route says so, after the Event's own checks.
	m, store = build(false, stubForm{form: questions})
	store.rows = map[[2]uuid.UUID]Mapping{{open.ID, form}: valid}
	if _, err := m.Questions(ctx, editor, open.ID, form); !errors.Is(err, ErrFormsPullOff) {
		t.Fatalf("questions off: %v", err)
	}
	if _, err := m.Questions(ctx, member, open.ID, form); !errors.Is(err, ErrMappingForbidden) {
		t.Fatalf("questions off, member: %v", err)
	}
	if _, err := m.Get(ctx, editor, open.ID, form); !errors.Is(err, ErrFormsPullOff) {
		t.Fatalf("get off: %v", err)
	}
	if err := m.Delete(ctx, editor, open.ID, form); !errors.Is(err, ErrFormsPullOff) || len(store.rows) != 1 {
		t.Fatalf("delete off: %v", err)
	}
	// A viewer in Forms reads the questions, maps, reads and deletes the
	// mapping (Yusuf, 2026-11-26); someone not on the team does none of it.
	m, store = build(true, stubForm{form: withTeam(team(RoleViewer), true)})
	if got, err := m.Questions(ctx, editor, open.ID, form); err != nil || len(got.Questions) != 3 {
		t.Fatalf("questions, viewer: %+v %v", got, err)
	}
	if _, created, err := m.Put(ctx, editor, valid); err != nil || !created {
		t.Fatalf("put, viewer: %t %v", created, err)
	}
	if got, err := m.Get(ctx, editor, open.ID, form); err != nil || got.Email != "q1" {
		t.Fatalf("get, viewer: %+v %v", got, err)
	}
	if err := m.Delete(ctx, editor, open.ID, form); err != nil || len(store.rows) != 0 {
		t.Fatalf("delete, viewer: %v", err)
	}
	m, store = build(true, stubForm{form: withTeam([]Collaborator{{UserID: uuid.New(), Role: RoleViewer}}, true)})
	store.rows = map[[2]uuid.UUID]Mapping{{open.ID, form}: valid}
	if _, err := m.Questions(ctx, editor, open.ID, form); !errors.Is(err, ErrFormsAccessDenied) {
		t.Fatalf("questions, not on the team: %v", err)
	}
	if _, err := m.Get(ctx, editor, open.ID, form); !errors.Is(err, ErrFormsAccessDenied) {
		t.Fatalf("get, not on the team: %v", err)
	}
	if err := m.Delete(ctx, editor, open.ID, form); !errors.Is(err, ErrFormsAccessDenied) || len(store.rows) != 1 {
		t.Fatalf("delete, not on the team: %v", err)
	}
	// A form Forms no longer has may lose its mapping.
	m, store = build(true, stubForm{err: ErrNotFound})
	store.rows = map[[2]uuid.UUID]Mapping{{open.ID, form}: valid}
	if err := m.Delete(ctx, editor, open.ID, form); err != nil || len(store.rows) != 0 {
		t.Fatalf("delete, form gone: %v", err)
	}
}

// Every Event's mapping of a form is read in one request to Forms: a save
// that takes the form's mappings past MaxFields questions is refused; the
// Event's own earlier mapping does not count (it is replaced).
func TestAMappingKeepsTheFormsQuestionsWithinOneRead(t *testing.T) {
	form := uuid.New()
	address := "https://forms.yildizskylab.com/" + form.String()
	ev := event.Event{ID: uuid.New(), OwnerTeam: "WEBLAB", Status: event.StatusPublished, FormURL: address}
	editor := authz.Principal{ID: uuid.NewString(), Groups: []string{"/UYELER/YK"}}
	var questions []Question
	for i := 1; i <= 30; i++ {
		questions = append(questions, Question{ID: fmt.Sprintf("q%d", i)})
	}
	store := &memMappings{}
	m := &Mappings{
		Events: backfillEvents{events: []event.Event{ev}}, Authz: authz.NewAuthorizer(authz.DefaultPolicy()), Store: store,
		Forms: stubForm{form: Form{FormID: form, Questions: questions, Collaborators: []Collaborator{{UserID: uuid.MustParse(editor.ID), Role: RoleEditor}}, AccessListed: true}},
		On:    true, Now: time.Now,
	}
	ctx := context.Background()
	// Two other Events' mappings name q1–q16 (16 questions).
	for _, other := range []Mapping{
		{EventID: uuid.New(), FormID: form, FirstName: "q1", LastName: "q2", Email: "q3", Phone: "q4", University: "q5", Department: "q6", Grade: "q7"},
		{EventID: uuid.New(), FormID: form, FirstName: "q8", LastName: "q9", Email: "q10", Phone: "q11", University: "q12", Department: "q13", Grade: "q14", FullName: "q15"},
	} {
		if _, _, err := store.Put(ctx, other); err != nil {
			t.Fatal(err)
		}
	}
	fits := Mapping{EventID: ev.ID, FormID: form, FullName: "q16", Email: "q17", Phone: "q18", Grade: "q19", University: "q20"}
	if _, _, err := m.Put(ctx, editor, fits); err != nil {
		t.Fatalf("20 questions: %v", err)
	}
	over := fits
	over.Department = "q21"
	if _, _, err := m.Put(ctx, editor, over); !errors.Is(err, ErrMappingTooManyQuestions) {
		t.Fatalf("21 questions: %v", err)
	}
	// The same questions as the others' count once.
	shared := fits
	shared.Department = "q6"
	if _, _, err := m.Put(ctx, editor, shared); err != nil {
		t.Fatalf("a question another Event maps too: %v", err)
	}
}
