package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authn"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/formsfeed"
)

type stubMappings struct {
	err     error
	created bool
	put     formsfeed.Mapping
}

func (s *stubMappings) Questions(_ context.Context, _ authz.Principal, _, formID uuid.UUID) (formsfeed.FormQuestions, error) {
	return formsfeed.FormQuestions{
		Form:       formsfeed.Form{FormID: formID, Title: "Başvuru", Questions: []formsfeed.Question{{ID: "q1", Type: "short_text", Question: "E-posta", Identity: "email"}}},
		Suggestion: formsfeed.Mapping{Email: "q1"},
	}, s.err
}

func (s *stubMappings) Get(_ context.Context, _ authz.Principal, eventID, formID uuid.UUID) (formsfeed.Mapping, error) {
	return formsfeed.Mapping{EventID: eventID, FormID: formID, FullName: "q2", Email: "q1", UpdatedAt: time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)}, s.err
}

func (s *stubMappings) Put(_ context.Context, _ authz.Principal, m formsfeed.Mapping) (formsfeed.Mapping, bool, error) {
	s.put = m
	return m, s.created, s.err
}

func (s *stubMappings) Delete(context.Context, authz.Principal, uuid.UUID, uuid.UUID) error {
	return s.err
}

func mappingApp(stub *stubMappings) *fiber.App {
	app := fiber.New()
	app.Use(func(c fiber.Ctx) error {
		c.Locals(authn.LocalsIdentity, authn.Identity{ID: uuid.New(), Groups: []string{"/UYELER/YK"}})
		return c.Next()
	})
	h := NewFormMappingHandler(stub, nil)
	app.Get("/v1/events/:id/forms/:formId/questions", h.Questions)
	app.Get("/v1/events/:id/forms/:formId/mapping", h.Get)
	app.Put("/v1/events/:id/forms/:formId/mapping", h.Put)
	app.Delete("/v1/events/:id/forms/:formId/mapping", h.Delete)
	return app
}

func mappingCall(t *testing.T, app *fiber.App, method, path, body string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func TestFormMappingAnswers(t *testing.T) {
	base := "/v1/events/" + uuid.NewString() + "/forms/" + uuid.NewString()
	stub := &stubMappings{}
	app := mappingApp(stub)

	status, body := mappingCall(t, app, "GET", base+"/questions", "")
	questions, _ := body["questions"].([]any)
	suggestion, _ := body["suggestion"].(map[string]any)
	if status != 200 || body["title"] != "Başvuru" || len(questions) != 1 || suggestion["email"] != "q1" || suggestion["firstName"] != nil {
		t.Fatalf("questions: %d %v", status, body)
	}
	status, body = mappingCall(t, app, "GET", base+"/mapping", "")
	if status != 200 || body["fullName"] != "q2" || body["email"] != "q1" || body["firstName"] != nil || body["updatedAt"] != "2026-11-25T12:00:00Z" {
		t.Fatalf("get: %d %v", status, body)
	}
	stub.created = true
	status, body = mappingCall(t, app, "PUT", base+"/mapping", `{"firstName":"q2","lastName":"q3","fullName":null,"email":"q1"}`)
	if status != 201 || body["firstName"] != "q2" || stub.put.LastName != "q3" || stub.put.FullName != "" {
		t.Fatalf("put: %d %v %+v", status, body, stub.put)
	}
	stub.created = false
	if status, _ := mappingCall(t, app, "PUT", base+"/mapping", `{"fullName":"q2","email":"q1"}`); status != 200 {
		t.Fatalf("replace: %d", status)
	}
	if status, _ := mappingCall(t, app, "PUT", base+"/mapping", `{"email":`); status != 400 {
		t.Fatalf("broken body: %d", status)
	}
	if status, _ := mappingCall(t, app, "DELETE", base+"/mapping", ""); status != 204 {
		t.Fatalf("delete: %d", status)
	}
	if status, _ := mappingCall(t, app, "GET", "/v1/events/x/forms/"+uuid.NewString()+"/mapping", ""); status != 400 {
		t.Fatalf("bad id: %d", status)
	}

	for _, tc := range []struct {
		err    error
		status int
		code   string
	}{
		{formsfeed.ErrMappingEventNotFound, 404, ""},
		{formsfeed.ErrMappingForbidden, 403, ""},
		{formsfeed.ErrMappingNotListed, 404, "form_not_listed"},
		{formsfeed.ErrMappingNotFound, 404, "mapping_not_found"},
		{formsfeed.ErrFormGone, 404, "form_not_found"},
		{formsfeed.ErrMappingInvalid, 400, "mapping_invalid"},
		{&formsfeed.MappingQuestionMissingError{Missing: []string{"q9"}}, 422, "mapping_question_missing"},
		{formsfeed.ErrFormsPullOff, 503, "forms_pull_off"},
		{errors.Join(formsfeed.ErrFormsUnavailable, formsfeed.ErrForbidden), 503, "forms_unavailable"},
		{errors.New("database"), 500, ""},
	} {
		stub.err = tc.err
		status, body := mappingCall(t, app, "PUT", base+"/mapping", `{"fullName":"q2","email":"q1"}`)
		if status != tc.status || (tc.code != "" && body["code"] != tc.code) {
			t.Fatalf("%v: %d %v", tc.err, status, body)
		}
		if tc.code == "mapping_question_missing" {
			if missing, _ := body["missing"].([]any); len(missing) != 1 || missing[0] != "q9" {
				t.Fatalf("missing: %v", body)
			}
		}
	}
}
