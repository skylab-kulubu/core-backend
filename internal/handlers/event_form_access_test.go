package handlers

import (
	"context"
	"errors"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/formsfeed"
)

// teamForms answers Forms' form read: viewed lists the caller as a viewer,
// down is a Forms that does not answer, any other form has another team.
type teamForms struct {
	person, viewed, down uuid.UUID
	reads                int
}

func (f *teamForms) Form(_ context.Context, id uuid.UUID) (formsfeed.Form, error) {
	f.reads++
	switch id {
	case f.down:
		return formsfeed.Form{}, errors.New("formsfeed: Forms answered 503")
	case f.viewed:
		return formsfeed.Form{FormID: id, AccessListed: true, Collaborators: []formsfeed.Collaborator{{UserID: f.person, Role: formsfeed.RoleViewer}}}, nil
	}
	return formsfeed.Form{FormID: id, AccessListed: true, Collaborators: []formsfeed.Collaborator{{UserID: uuid.New(), Role: formsfeed.RoleOwner}}}, nil
}

// Linking a form the caller is not on the team of in Forms is 403
// forms_access_denied, a Forms that cannot tell 503 forms_unavailable; both
// write nothing. A viewer links; a write that keeps the forms reads no Forms.
func TestEventFormAccessAnswers(t *testing.T) {
	t.Parallel()
	ident := yk()
	forms := &teamForms{person: ident.ID, viewed: uuid.New(), down: uuid.New()}
	theirs := uuid.New()
	store := event.NewMemoryStore()
	svc := event.NewServiceWithOptions(store, authz.NewAuthorizer(authz.DefaultPolicy()), event.ServiceOptions{
		FormAccess: &formsfeed.FormAccess{Forms: forms},
	})
	app := eventServiceApp(t, ident, svc)
	address := func(id uuid.UUID) string { return "https://forms.yildizskylab.com/f/" + id.String() }

	status, out := sendEventJSON(t, app, fiber.MethodPost, "/v1/events", `{"name":"Gecekodu","location":"YTÜ","ownerTeam":"WEBLAB","formUrl":"`+address(theirs)+`"}`)
	if status != fiber.StatusForbidden || out["code"] != "forms_access_denied" {
		t.Fatalf("create, another's form: %d %v", status, out)
	}
	status, out = sendEventJSON(t, app, fiber.MethodPost, "/v1/events", `{"name":"Gecekodu","location":"YTÜ","ownerTeam":"WEBLAB","formUrl":"`+address(forms.down)+`"}`)
	if status != fiber.StatusServiceUnavailable || out["code"] != "forms_unavailable" {
		t.Fatalf("create, Forms down: %d %v", status, out)
	}
	if all, err := store.ListIncludingDrafts(context.Background(), ""); err != nil || len(all) != 0 {
		t.Fatalf("a refused create wrote %d (%v)", len(all), err)
	}

	status, created := sendEventJSON(t, app, fiber.MethodPost, "/v1/events", `{"name":"Gecekodu","location":"YTÜ","ownerTeam":"WEBLAB","formUrl":"`+address(forms.viewed)+`"}`)
	if status != fiber.StatusCreated {
		t.Fatalf("create, a viewer: %d %v", status, created)
	}
	path := "/v1/events/" + created["id"].(string)
	status, out = sendEventJSON(t, app, fiber.MethodPatch, path, `{"extraFormUrls":[{"label":"CTF","url":"`+address(theirs)+`"}]}`)
	if status != fiber.StatusForbidden || out["code"] != "forms_access_denied" {
		t.Fatalf("patch, another's form: %d %v", status, out)
	}
	status, out = sendEventJSON(t, app, fiber.MethodPut, path, `{"formUrl":"`+address(forms.down)+`"}`)
	if status != fiber.StatusServiceUnavailable || out["code"] != "forms_unavailable" {
		t.Fatalf("put, Forms down: %d %v", status, out)
	}
	reads := forms.reads
	status, out = sendEventJSON(t, app, fiber.MethodPatch, path, `{"name":"Gecekodu 2","formUrl":"`+address(forms.viewed)+`?utm_source=x"}`)
	if status != fiber.StatusOK || out["formUrl"] != address(forms.viewed)+"?utm_source=x" || len(out["extraFormUrls"].([]any)) != 0 {
		t.Fatalf("patch keeping the form: %d %v", status, out)
	}
	if forms.reads != reads {
		t.Fatalf("a kept form read Forms %d times", forms.reads-reads)
	}
}
