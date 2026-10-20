package handlers

import (
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authn"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// Registration closure (ADR 0066 addendum 2026-10-19, ticket 12): applying
// closed by hand or by a deadline, apart from the status.

var (
	closureMember = authn.Identity{ID: uuid.MustParse("c0000000-0000-0000-0000-00000000000b"), Groups: []string{"/UYELER/ARGE/WEBLAB"}}
	closureTarget = uuid.MustParse("c0000000-0000-0000-0000-0000000000aa")
	closureForms  = authn.Identity{ID: uuid.MustParse("c0000000-0000-0000-0000-0000000000f0"), Client: "forms", ServiceAccount: true, Product: authz.ProductForms, Roles: []string{"ticket:forms"}}
)

type closureEnv struct {
	events  *event.MemoryStore
	tickets *ticket.MemoryStore
	users   *user.MemoryStore
}

func newClosureEnv(t *testing.T) *closureEnv {
	t.Helper()
	e := &closureEnv{events: event.NewMemoryStore(), tickets: ticket.NewMemoryStore(), users: user.NewMemoryStore()}
	seedTicketUser(t, e.users, closureTarget)
	seedTicketUser(t, e.users, closureMember.ID)
	return e
}

// create makes a published Event (or a draft) that starts in two days, or
// that ended (started three days ago), closed as asked.
func (e *closureEnv) create(t *testing.T, name string, status event.Status, ended, closed bool, closesAt *time.Time, formID uuid.UUID) event.Event {
	t.Helper()
	start := time.Now().Add(48 * time.Hour)
	if ended {
		start = time.Now().Add(-72 * time.Hour)
	}
	in := event.Event{
		Name: name, Location: "YTÜ", OwnerTeam: "WEBLAB", Status: status, StartDate: &start,
		RegistrationClosed: closed, RegistrationClosesAt: closesAt,
	}
	if formID != uuid.Nil {
		in.FormURL = "https://forms.yildizskylab.com/" + formID.String()
	}
	created, err := e.events.Create(t.Context(), in)
	if err != nil {
		t.Fatal(err)
	}
	return created
}

func (e *closureEnv) app(t *testing.T, ident authn.Identity) *fiber.App {
	t.Helper()
	az := authz.NewAuthorizer(authz.DefaultPolicy())
	eh := NewEventHandler(event.NewService(e.events, az))
	th := NewTicketHandler(ticket.NewService(e.tickets, e.events, az, e.users))
	app := fiber.New()
	app.Use(func(c fiber.Ctx) error {
		if ident.ID != uuid.Nil || len(ident.Groups) > 0 {
			c.Locals(authn.LocalsIdentity, ident)
		}
		return c.Next()
	})
	app.Post("/v1/events", eh.Create)
	app.Get("/v1/events/:id", eh.Get)
	app.Patch("/v1/events/:id", eh.Update)
	app.Post("/v1/events/:eventId/applications/me", th.Apply)
	app.Post("/v1/events/:eventId/applications/users/:userId", th.ApplyForOther)
	app.Post("/v1/events/:eventId/applications/guest", th.ApplyGuest)
	app.Post("/v1/forms/:formId/responses", th.RecordFormResponse)
	return app
}

func (e *closureEnv) ticketCount(t *testing.T, eventID uuid.UUID) int {
	t.Helper()
	listed, err := e.tickets.ListByEvent(t.Context(), eventID)
	if err != nil {
		t.Fatal(err)
	}
	return len(listed)
}

func closureGuestBody(email string) string {
	return `{"firstName":"Ada","lastName":"Lovelace","email":"` + email + `"}`
}

func TestRegistrationClosureRefusesSelfApplyOnly(t *testing.T) {
	t.Parallel()
	env := newClosureEnv(t)
	past, future := time.Now().Add(-time.Hour), time.Now().Add(time.Hour)
	cases := []struct {
		name string
		ev   event.Event
		open bool
	}{
		{"open", env.create(t, "Açık", event.StatusPublished, false, false, nil, uuid.Nil), true},
		{"deadline to come", env.create(t, "Son tarih gelmedi", event.StatusPublished, false, false, &future, uuid.Nil), true},
		{"closed by hand", env.create(t, "Elle kapalı", event.StatusPublished, false, true, nil, uuid.Nil), false},
		{"deadline gone", env.create(t, "Son tarih geçti", event.StatusPublished, false, false, &past, uuid.Nil), false},
		{"ended", env.create(t, "Bitti", event.StatusPublished, true, false, nil, uuid.Nil), false},
		{"cancelled", env.create(t, "İptal", event.StatusCancelled, false, false, nil, uuid.Nil), false},
	}
	member, anon, leader := env.app(t, closureMember), env.app(t, authn.Identity{}), env.app(t, weblabLeader())
	for _, tc := range cases {
		path := "/v1/events/" + tc.ev.ID.String()
		_, got := sendEventJSON(t, leader, fiber.MethodGet, path, "")
		if got["registrationOpen"] != tc.open {
			t.Errorf("%s: registrationOpen %v, want %v", tc.name, got["registrationOpen"], tc.open)
		}

		wantStatus, wantCode := fiber.StatusCreated, ""
		if !tc.open {
			wantStatus, wantCode = fiber.StatusConflict, "registration_closed"
		}
		for who, call := range map[string]func() (int, map[string]any){
			"me": func() (int, map[string]any) {
				return sendEventJSON(t, member, fiber.MethodPost, path+"/applications/me", "")
			},
			"guest": func() (int, map[string]any) {
				return sendEventJSON(t, anon, fiber.MethodPost, path+"/applications/guest", closureGuestBody("misafir@example.com"))
			},
		} {
			status, body := call()
			if status != wantStatus || (wantCode != "" && body["code"] != wantCode) {
				t.Errorf("%s %s: %d %v, want %d %s", tc.name, who, status, body, wantStatus, wantCode)
			}
		}
		// A member's own guest application is refused like any other.
		if status, body := sendEventJSON(t, member, fiber.MethodPost, path+"/applications/guest", closureGuestBody("uye-misafir@example.com")); status != wantStatus || (wantCode != "" && body["code"] != wantCode) {
			t.Errorf("%s member's guest: %d %v", tc.name, status, body)
		}
		// The closure stops a person's own application only: the editor's
		// own one too, but not adding a person or a guest from the panel.
		if status, body := sendEventJSON(t, leader, fiber.MethodPost, path+"/applications/me", ""); status != wantStatus || (wantCode != "" && body["code"] != wantCode) {
			t.Errorf("%s editor's own: %d %v", tc.name, status, body)
		}
		if tc.ev.Status == event.StatusPublished {
			if status, body := sendEventJSON(t, leader, fiber.MethodPost, path+"/applications/users/"+closureTarget.String(), ""); status != fiber.StatusCreated {
				t.Errorf("%s panel add: %d %v", tc.name, status, body)
			}
			if status, body := sendEventJSON(t, leader, fiber.MethodPost, path+"/applications/guest", closureGuestBody("panel-misafir@example.com")); status != fiber.StatusCreated {
				t.Errorf("%s panel guest: %d %v", tc.name, status, body)
			}
		}
	}
}

func TestRegistrationClosureIsWrittenByPatch(t *testing.T) {
	t.Parallel()
	env := newClosureEnv(t)
	leader := env.app(t, weblabLeader())
	status, created := sendEventJSON(t, leader, fiber.MethodPost, "/v1/events",
		`{"name":"Gecekodu","location":"YTÜ","ownerTeam":"WEBLAB","startDate":"`+time.Now().Add(48*time.Hour).UTC().Format(time.RFC3339)+`","ranked":true}`)
	if status != fiber.StatusCreated || created["registrationOpen"] != true || created["registrationClosed"] != false {
		t.Fatalf("create %d %v", status, created)
	}
	path := "/v1/events/" + created["id"].(string)
	member := env.app(t, closureMember)
	past := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	for _, step := range []struct {
		body   string
		status int
		open   any // registrationOpen after the step; nil: the step is refused
		closed any
	}{
		{`{"registrationClosed":true}`, fiber.StatusOK, false, true},
		// Another field leaves the closure as it is (ticket 13).
		{`{"name":"Gecekodu 2"}`, fiber.StatusOK, false, true},
		{`{"registrationClosed":null}`, fiber.StatusBadRequest, nil, nil},
		{`{"registrationClosed":false}`, fiber.StatusOK, true, false},
		{`{"registrationClosesAt":"` + past + `"}`, fiber.StatusOK, false, false},
		{`{"registrationClosesAt":null}`, fiber.StatusOK, true, false},
	} {
		status, got := sendEventJSON(t, leader, fiber.MethodPatch, path, step.body)
		if status != step.status {
			t.Fatalf("%s: %d %v", step.body, status, got)
		}
		if step.open == nil {
			continue
		}
		if got["registrationOpen"] != step.open || got["registrationClosed"] != step.closed || got["ranked"] != true {
			t.Fatalf("%s: %v", step.body, got)
		}
		if _, present := got["registrationClosesAt"]; present != (step.body == `{"registrationClosesAt":"`+past+`"}`) {
			t.Fatalf("%s: registrationClosesAt %v", step.body, got["registrationClosesAt"])
		}
	}
	// A member may not close it.
	if status, got := sendEventJSON(t, member, fiber.MethodPatch, path, `{"registrationClosed":true}`); status != fiber.StatusForbidden {
		t.Fatalf("member: %d %v", status, got)
	}
}

func TestFormsAnswerToAClosedRegistrationWritesNoTicket(t *testing.T) {
	t.Parallel()
	env := newClosureEnv(t)
	past := time.Now().Add(-time.Hour)
	closedForm, deadlineForm, sharedForm, draftForm, endedForm := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	closed := env.create(t, "Elle kapalı", event.StatusPublished, false, true, nil, closedForm)
	deadline := env.create(t, "Son tarih geçti", event.StatusPublished, false, false, &past, deadlineForm)
	sharedClosed := env.create(t, "Ortak kapalı", event.StatusPublished, false, true, nil, sharedForm)
	sharedOpen := env.create(t, "Ortak açık", event.StatusPublished, false, false, nil, sharedForm)
	draft := env.create(t, "Taslak kapalı", event.StatusDraft, false, true, nil, draftForm)
	ended := env.create(t, "Bitti", event.StatusPublished, true, false, nil, endedForm)
	forms := env.app(t, closureForms)
	report := func(formID uuid.UUID) (int, map[string]any) {
		return sendEventJSON(t, forms, fiber.MethodPost, "/v1/forms/"+formID.String()+"/responses",
			`{"responseId":"`+uuid.NewString()+`","status":"accepted","userId":"`+closureTarget.String()+`"}`)
	}
	for _, tc := range []struct {
		name string
		form uuid.UUID
		ev   event.Event
	}{
		{"closed by hand", closedForm, closed},
		{"deadline gone", deadlineForm, deadline},
		{"draft closed by hand", draftForm, draft},
	} {
		status, body := report(tc.form)
		if status != fiber.StatusOK || body["outcome"] != "registration_closed" {
			t.Errorf("%s: %d %v", tc.name, status, body)
		}
		if n := env.ticketCount(t, tc.ev.ID); n != 0 {
			t.Errorf("%s: %d Tickets", tc.name, n)
		}
	}
	// A form two Events list writes the open one's Ticket alone.
	if status, body := report(sharedForm); status != fiber.StatusNoContent {
		t.Fatalf("shared: %d %v", status, body)
	}
	if env.ticketCount(t, sharedOpen.ID) != 1 || env.ticketCount(t, sharedClosed.ID) != 0 {
		t.Fatalf("shared: open %d closed %d", env.ticketCount(t, sharedOpen.ID), env.ticketCount(t, sharedClosed.ID))
	}
	// An ended Event the organizer did not close takes the answer as before.
	if status, body := report(endedForm); status != fiber.StatusNoContent || env.ticketCount(t, ended.ID) != 1 {
		t.Fatalf("ended: %d %v, %d Tickets", status, body, env.ticketCount(t, ended.ID))
	}
}
