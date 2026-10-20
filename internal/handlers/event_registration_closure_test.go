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
		// A cancelled Event says so first, to everyone (ticket 04).
		if tc.ev.Status == event.StatusCancelled {
			wantCode = "event_cancelled"
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
		// The operator of a cancelled Event adds nobody, not a guest either
		// (review B2).
		if tc.ev.Status == event.StatusCancelled {
			if status, body := sendEventJSON(t, leader, fiber.MethodPost, path+"/applications/guest", closureGuestBody("panel-misafir@example.com")); status != fiber.StatusConflict || body["code"] != "event_cancelled" {
				t.Errorf("%s panel guest: %d %v", tc.name, status, body)
			}
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
	var closedAt any
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
		// When applying was closed by hand is kept while it stays closed.
		if _, at := got["registrationClosedAt"]; at != (step.closed == true) {
			t.Fatalf("%s: registrationClosedAt %v", step.body, got["registrationClosedAt"])
		}
		if step.closed == true {
			if closedAt == nil {
				closedAt = got["registrationClosedAt"]
			} else if got["registrationClosedAt"] != closedAt {
				t.Fatalf("%s: registrationClosedAt moved %v → %v", step.body, closedAt, got["registrationClosedAt"])
			}
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

// Closing applying does not refuse a Forms report (Yusuf, 2026-10-20,
// review B1): an answer sent in time and approved later, or reported late,
// earns its Ticket. Only a report that says when the answer was sent
// (submittedAt) is judged by it, against when applying was closed by hand
// or by the deadline. A cancelled Event is refused first, as before.
func TestFormsAnswerToAClosedRegistration(t *testing.T) {
	t.Parallel()
	env := newClosureEnv(t)
	now := time.Now()
	past := now.Add(-time.Hour)
	closedForm, deadlineForm, sharedForm, cancelledForm, endedForm := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	closed := env.create(t, "Elle kapalı", event.StatusPublished, false, true, nil, closedForm)
	deadline := env.create(t, "Son tarih geçti", event.StatusPublished, false, false, &past, deadlineForm)
	sharedClosed := env.create(t, "Ortak kapalı", event.StatusPublished, false, true, nil, sharedForm)
	sharedOpen := env.create(t, "Ortak açık", event.StatusPublished, false, false, nil, sharedForm)
	cancelled := env.create(t, "İptal ve kapalı", event.StatusCancelled, false, true, nil, cancelledForm)
	ended := env.create(t, "Bitti", event.StatusPublished, true, false, nil, endedForm)
	forms := env.app(t, closureForms)
	report := func(formID uuid.UUID, submitted *time.Time) (int, map[string]any) {
		at := ""
		if submitted != nil {
			at = `,"submittedAt":"` + submitted.UTC().Format(time.RFC3339Nano) + `"`
		}
		return sendEventJSON(t, forms, fiber.MethodPost, "/v1/forms/"+formID.String()+"/responses",
			`{"responseId":"`+uuid.NewString()+`","status":"accepted","guest":{"firstName":"Ada","lastName":"Lovelace","email":"`+uuid.NewString()+`@example.com"}`+at+`}`)
	}
	before, after := now.Add(-2*time.Hour), now.Add(time.Minute)
	for _, tc := range []struct {
		name      string
		form      uuid.UUID
		ev        event.Event
		submitted *time.Time
		outcome   string // "" writes the Ticket (204)
	}{
		{"closed by hand, no submittedAt", closedForm, closed, nil, ""},
		{"closed by hand, sent before", closedForm, closed, &before, ""},
		{"closed by hand, sent after", closedForm, closed, &after, "registration_closed"},
		{"deadline gone, no submittedAt", deadlineForm, deadline, nil, ""},
		{"deadline gone, sent before", deadlineForm, deadline, &before, ""},
		{"deadline gone, sent after", deadlineForm, deadline, &after, "registration_closed"},
		{"cancelled", cancelledForm, cancelled, nil, "event_cancelled"},
		{"cancelled, sent after closing", cancelledForm, cancelled, &after, "event_cancelled"},
		{"ended", endedForm, ended, &after, ""},
	} {
		count := env.ticketCount(t, tc.ev.ID)
		status, body := report(tc.form, tc.submitted)
		wrote := env.ticketCount(t, tc.ev.ID) - count
		switch {
		case tc.outcome == "" && (status != fiber.StatusNoContent || wrote != 1):
			t.Errorf("%s: %d %v, %d Tickets", tc.name, status, body, wrote)
		case tc.outcome != "" && (status != fiber.StatusOK || body["outcome"] != tc.outcome || wrote != 0):
			t.Errorf("%s: %d %v, %d Tickets", tc.name, status, body, wrote)
		}
	}
	// A form two Events list: an answer sent after one closed writes the
	// other's Ticket alone.
	if status, body := report(sharedForm, &after); status != fiber.StatusNoContent {
		t.Fatalf("shared: %d %v", status, body)
	}
	if env.ticketCount(t, sharedOpen.ID) != 1 || env.ticketCount(t, sharedClosed.ID) != 0 {
		t.Fatalf("shared: open %d closed %d", env.ticketCount(t, sharedOpen.ID), env.ticketCount(t, sharedClosed.ID))
	}
}
