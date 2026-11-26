package handlers

import (
	"encoding/json"
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authn"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/clientip"
	"github.com/skylab-kulubu/core-backend/internal/doorqr"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
)

// The Ticket, door, import and guest apply routes say why they refuse a
// request (docs/problem-codes.md): every 400 and 409 of theirs carries a
// code, and the codes and statuses they had already stay as they were.

type codedAnswer struct {
	status int
	body   map[string]any
	raw    string
}

func (a codedAnswer) code() string {
	code, _ := a.body["code"].(string)
	return code
}

func callCoded(t *testing.T, app *fiber.App, method, path, body string, headers ...string) codedAnswer {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	var decoded map[string]any
	_ = json.Unmarshal(raw, &decoded)
	return codedAnswer{status: resp.StatusCode, body: decoded, raw: string(raw)}
}

func wantCode(t *testing.T, name string, got codedAnswer, status int, code string) {
	t.Helper()
	if got.status != status || got.code() != code {
		t.Fatalf("%s: want %d %s, got %d %s", name, status, code, got.status, got.raw)
	}
}

// ticketCodesApp serves every route of the Ticket handler these tests call.
func ticketCodesApp(t *testing.T, ident authn.Identity, events event.Store, tickets ticket.Store, extras ...any) *fiber.App {
	t.Helper()
	h := NewTicketHandler(ticket.NewService(tickets, events, authz.NewAuthorizer(authz.DefaultPolicy()), extras...))
	h.logger = nil
	app := fiber.New(fiber.Config{ErrorHandler: ErrorHandler})
	app.Use(func(c fiber.Ctx) error {
		if ident.ID != uuid.Nil || len(ident.Groups) > 0 || ident.ServiceAccount {
			c.Locals(authn.LocalsIdentity, ident)
		}
		return c.Next()
	})
	app.Post("/v1/events/:eventId/applications/me", h.Apply)
	app.Post("/v1/events/:eventId/applications/users/:userId", h.ApplyForOther)
	app.Post("/v1/events/:eventId/applications/guest", h.ApplyGuest)
	app.Get("/v1/events/:eventId/tickets", h.ListByEvent)
	app.Get("/v1/tickets/me", h.Mine)
	app.Get("/v1/tickets/:id", h.Get)
	app.Get("/v1/tickets", h.List)
	app.Post("/v1/tickets/search", h.Search)
	app.Post("/v1/tickets/:ticketId/sessions/:sessionId/check-in", h.CheckIn)
	app.Post("/v1/sessions/:sessionId/check-in/me", h.CheckInMe)
	app.Post("/v1/sessions/:sessionId/check-in/guest", h.CheckInGuest)
	app.Post("/v1/sessions/:sessionId/door-qr", h.MintDoorQR)
	app.Post("/v1/sessions/:sessionId/check-in/resolve", h.ResolveAndCheckIn)
	app.Post("/v1/tickets/:id/cancel", h.CancelTicket)
	app.Post("/v1/tickets/:id/notification", h.ResendTicket)
	app.Post("/v1/tickets/:id/access-key/rotate", h.RenewTicketLink)
	app.Post("/v1/events/:eventId/attendee-imports/preview", h.PreviewImport)
	app.Post("/v1/events/:eventId/attendee-imports", h.ImportAttendees)
	app.Post("/v1/events/:eventId/entries/resolve", h.ResolveEntry)
	app.Get("/v1/events/:eventId/entries", h.ListEntries)
	app.Post("/v1/forms/:formId/responses", h.RecordFormResponse)
	return app
}

type ticketCodesFixture struct {
	events  *event.MemoryStore
	tickets *ticket.MemoryStore
	ev      event.Event
	sess    event.Session
	other   event.Session
	leader  authn.Identity
}

func newTicketCodesFixture(t *testing.T) ticketCodesFixture {
	t.Helper()
	ctx := t.Context()
	events := event.NewMemoryStore()
	tickets := ticket.NewMemoryStore()
	ev, err := events.Create(ctx, event.Event{Name: "Hack", Location: "YTÜ", OwnerTeam: "WEBLAB", FormURL: "https://forms.yildizskylab.com/f/hack"})
	if err != nil {
		t.Fatal(err)
	}
	day, err := events.CreateDay(ctx, event.Day{EventID: ev.ID, Name: "Gün 1"})
	if err != nil {
		t.Fatal(err)
	}
	sess, err := events.CreateSession(ctx, event.Session{EventDayID: day.ID, Title: "Açılış", SpeakerName: "Ada", SessionType: "PRESENTATION"})
	if err != nil {
		t.Fatal(err)
	}
	otherEv, err := events.Create(ctx, event.Event{Name: "Jam", Location: "YTÜ", OwnerTeam: "WEBLAB"})
	if err != nil {
		t.Fatal(err)
	}
	otherDay, err := events.CreateDay(ctx, event.Day{EventID: otherEv.ID, Name: "Gün 1"})
	if err != nil {
		t.Fatal(err)
	}
	other, err := events.CreateSession(ctx, event.Session{EventDayID: otherDay.ID, Title: "Başka", SpeakerName: "Grace", SessionType: "PRESENTATION"})
	if err != nil {
		t.Fatal(err)
	}
	return ticketCodesFixture{
		events: events, tickets: tickets, ev: ev, sess: sess, other: other,
		leader: authn.Identity{ID: uuid.New(), Groups: []string{"/UYELER/ARGE/WEBLAB/LIDERLER"}},
	}
}

func (f ticketCodesFixture) guest(t *testing.T, email string, status ticket.Status) ticket.Ticket {
	t.Helper()
	created, err := f.tickets.Create(t.Context(), ticket.Ticket{
		EventID: f.ev.ID, TicketType: ticket.Guest, GuestFirstName: "Ada", GuestLastName: "Lovelace", GuestEmail: email, Status: status,
	})
	if err != nil {
		t.Fatal(err)
	}
	return created
}

// The guest's own check-in at the door (skylab-site /kapi): a cancelled or
// waitlisted Ticket is 409 with its own code, not the earlier check-in's,
// which now has one too.
func TestGuestCheckInCodes(t *testing.T) {
	t.Parallel()
	f := newTicketCodesFixture(t)
	f.guest(t, "cancelled@example.com", ticket.StatusCancelled)
	f.guest(t, "waiting@example.com", ticket.StatusWaitlisted)
	f.guest(t, "ada@example.com", "")
	gate := doorqr.NewGate([]byte("handler door qr key, handler doo"), doorqr.Config{})
	pass, err := gate.MintFor(doorqr.ClassDoor, f.sess.ID, f.ev.ID)
	if err != nil {
		t.Fatal(err)
	}
	app := ticketCodesApp(t, authn.Identity{}, f.events, f.tickets, gate)
	path := "/v1/sessions/" + f.sess.ID.String() + "/check-in/guest"
	post := func(body string) codedAnswer { return callCoded(t, app, fiber.MethodPost, path, body) }

	wantCode(t, "cancelled", post(`{"email":"cancelled@example.com"}`), fiber.StatusConflict, "ticket_cancelled")
	wantCode(t, "waitlisted", post(`{"email":"waiting@example.com"}`), fiber.StatusConflict, "ticket_waitlisted")
	if got := post(`{"email":"ada@example.com"}`); got.status != fiber.StatusCreated {
		t.Fatalf("first check-in: %d %s", got.status, got.raw)
	}
	wantCode(t, "again", post(`{"email":"ada@example.com"}`), fiber.StatusConflict, "already_checked_in")
	wantCode(t, "neither", post(`{}`), fiber.StatusBadRequest, "email_or_ticket_key_required")

	got := post(`{"email":"nobody@example.com","doorToken":"` + pass.Token + `"}`)
	wantCode(t, "no ticket", got, fiber.StatusNotFound, "guest_ticket_not_found")
	if got.body["registrationUrl"] != "https://forms.yildizskylab.com/f/hack" {
		t.Fatalf("no ticket lost its registrationUrl: %s", got.raw)
	}
	got = post(`{"email":"nobody@example.com"}`)
	wantCode(t, "no ticket, no token", got, fiber.StatusNotFound, "guest_ticket_not_found")
	if _, ok := got.body["registrationUrl"]; ok {
		t.Fatalf("no token, yet a registrationUrl: %s", got.raw)
	}

	got = callCoded(t, app, fiber.MethodPost, "/v1/sessions/not-a-uuid/check-in/guest", `{"email":"ada@example.com"}`)
	wantCode(t, "bad session id", got, fiber.StatusBadRequest, "invalid_id")
	if got.body["param"] != "sessionId" {
		t.Fatalf("bad session id names no param: %s", got.raw)
	}
	wantCode(t, "bad body", post(`{"email":`), fiber.StatusBadRequest, "invalid_body")
}

// Guest apply: the missing fields are named.
func TestGuestApplyFieldsRequiredCode(t *testing.T) {
	t.Parallel()
	f := newTicketCodesFixture(t)
	app := ticketCodesApp(t, authn.Identity{}, f.events, f.tickets)
	path := "/v1/events/" + f.ev.ID.String() + "/applications/guest"
	got := callCoded(t, app, fiber.MethodPost, path, `{"firstName":"Ada","lastName":" ","email":""}`)
	wantCode(t, "missing", got, fiber.StatusBadRequest, "guest_fields_required")
	fields, _ := got.body["fields"].([]any)
	if len(fields) != 2 || fields[0] != "lastName" || fields[1] != "email" {
		t.Fatalf("fields: %s", got.raw)
	}
	wantCode(t, "bad body", callCoded(t, app, fiber.MethodPost, path, `{"firstName":`), fiber.StatusBadRequest, "invalid_body")
	got = callCoded(t, app, fiber.MethodPost, "/v1/events/x/applications/guest", `{}`)
	wantCode(t, "bad event id", got, fiber.StatusBadRequest, "invalid_id")
	if got.body["param"] != "eventId" {
		t.Fatalf("param: %s", got.raw)
	}
}

// The desk (session and Event door): no target, a target it cannot read,
// a name of two people.
func TestDoorDeskCodes(t *testing.T) {
	t.Parallel()
	f := newTicketCodesFixture(t)
	f.guest(t, "ada@example.com", "")
	f.guest(t, "ada2@example.com", "")
	app := ticketCodesApp(t, f.leader, f.events, f.tickets)
	resolve := "/v1/sessions/" + f.sess.ID.String() + "/check-in/resolve"
	entries := "/v1/events/" + f.ev.ID.String() + "/entries"

	wantCode(t, "session, no target", callCoded(t, app, fiber.MethodPost, resolve, `{"query":"  "}`), fiber.StatusBadRequest, "door_query_required")
	wantCode(t, "session, bad person", callCoded(t, app, fiber.MethodPost, resolve, `{"personId":"x"}`), fiber.StatusBadRequest, "door_target_invalid")
	wantCode(t, "session, bad body", callCoded(t, app, fiber.MethodPost, resolve, `{"query":`), fiber.StatusBadRequest, "invalid_body")
	got := callCoded(t, app, fiber.MethodPost, resolve, `{"query":"Ada Lovelace"}`)
	wantCode(t, "session, two people", got, fiber.StatusConflict, "ambiguous_match")
	if got.body["title"] != "Ambiguous Match" {
		t.Fatalf("the title changed: %s", got.raw)
	}

	wantCode(t, "event, no target", callCoded(t, app, fiber.MethodPost, entries+"/resolve", `{}`), fiber.StatusBadRequest, "door_query_required")
	wantCode(t, "event, bad person", callCoded(t, app, fiber.MethodPost, entries+"/resolve", `{"personId":"x"}`), fiber.StatusBadRequest, "door_target_invalid")
	wantCode(t, "event, bad body", callCoded(t, app, fiber.MethodPost, entries+"/resolve", `{"query":`), fiber.StatusBadRequest, "invalid_body")
	wantCode(t, "event, two people", callCoded(t, app, fiber.MethodPost, entries+"/resolve", `{"query":"Ada Lovelace"}`), fiber.StatusConflict, "ambiguous_match")

	wantCode(t, "entry date", callCoded(t, app, fiber.MethodGet, entries+"?date=2026-13-40", ""), fiber.StatusBadRequest, "entry_date_invalid")
	wantCode(t, "entry date shape", callCoded(t, app, fiber.MethodGet, entries+"?date=yesterday", ""), fiber.StatusBadRequest, "entry_date_invalid")
	got = callCoded(t, app, fiber.MethodGet, "/v1/events/x/entries", "")
	wantCode(t, "entries, bad event id", got, fiber.StatusBadRequest, "invalid_id")
	if got.body["param"] != "eventId" {
		t.Fatalf("param: %s", got.raw)
	}
}

// The operator's check-in, cancel, resend, list and door QR.
func TestTicketOperatorCodes(t *testing.T) {
	t.Parallel()
	f := newTicketCodesFixture(t)
	held := f.guest(t, "ada@example.com", "")
	app := ticketCodesApp(t, f.leader, f.events, f.tickets)
	id := held.ID.String()

	wantCode(t, "another Event's session",
		callCoded(t, app, fiber.MethodPost, "/v1/tickets/"+id+"/sessions/"+f.other.ID.String()+"/check-in", ""),
		fiber.StatusBadRequest, "session_not_in_event")

	got := callCoded(t, app, fiber.MethodPost, "/v1/tickets/"+id+"/cancel", `{"reason":"`+strings.Repeat("ç", ticket.MaxCancelReason+1)+`"}`)
	wantCode(t, "long reason", got, fiber.StatusBadRequest, "cancel_reason_too_long")
	if got.body["max"] != float64(ticket.MaxCancelReason) {
		t.Fatalf("max: %s", got.raw)
	}
	wantCode(t, "cancel, bad body", callCoded(t, app, fiber.MethodPost, "/v1/tickets/"+id+"/cancel", `{"reason":`), fiber.StatusBadRequest, "invalid_body")
	got = callCoded(t, app, fiber.MethodPost, "/v1/tickets/x/cancel", "")
	wantCode(t, "cancel, bad id", got, fiber.StatusBadRequest, "invalid_id")
	if got.body["param"] != "id" {
		t.Fatalf("param: %s", got.raw)
	}

	long := strings.Repeat("k", 129)
	wantCode(t, "resend, long key", callCoded(t, app, fiber.MethodPost, "/v1/tickets/"+id+"/notification", "", "Idempotency-Key", long),
		fiber.StatusBadRequest, "request_key_too_long")
	wantCode(t, "renew, long key", callCoded(t, app, fiber.MethodPost, "/v1/tickets/"+id+"/access-key/rotate", "", "Idempotency-Key", long),
		fiber.StatusBadRequest, "request_key_too_long")
	got = callCoded(t, app, fiber.MethodPost, "/v1/tickets/"+id+"/access-key/rotate?notify=maybe", "")
	wantCode(t, "renew, notify", got, fiber.StatusBadRequest, "invalid_query")
	if got.body["param"] != "notify" {
		t.Fatalf("param: %s", got.raw)
	}

	wantCode(t, "list, nothing asked", callCoded(t, app, fiber.MethodGet, "/v1/tickets", ""), fiber.StatusBadRequest, "ticket_query_required")
	wantCode(t, "list, bad user", callCoded(t, app, fiber.MethodGet, "/v1/tickets?userId=x", ""), fiber.StatusBadRequest, "user_id_invalid")
	wantCode(t, "search, bad body", callCoded(t, app, fiber.MethodPost, "/v1/tickets/search", `{"email":`), fiber.StatusBadRequest, "invalid_body")
	got = callCoded(t, app, fiber.MethodGet, "/v1/events/"+f.ev.ID.String()+"/tickets?status=gone", "")
	wantCode(t, "event list, status", got, fiber.StatusBadRequest, "invalid_query")
	if got.body["param"] != "status" {
		t.Fatalf("param: %s", got.raw)
	}
	got = callCoded(t, app, fiber.MethodGet, "/v1/tickets/me?include=gone", "")
	wantCode(t, "mine, include", got, fiber.StatusBadRequest, "invalid_query")
	if got.body["param"] != "include" {
		t.Fatalf("param: %s", got.raw)
	}
	got = callCoded(t, app, fiber.MethodPost, "/v1/sessions/"+f.sess.ID.String()+"/door-qr?use=wall", "")
	wantCode(t, "door qr, use", got, fiber.StatusBadRequest, "invalid_query")
	if got.body["param"] != "use" {
		t.Fatalf("param: %s", got.raw)
	}
	got = callCoded(t, app, fiber.MethodPost, "/v1/events/"+f.ev.ID.String()+"/applications/users/x", "")
	wantCode(t, "apply for other, bad user", got, fiber.StatusBadRequest, "invalid_id")
	if got.body["param"] != "userId" {
		t.Fatalf("param: %s", got.raw)
	}
	wantCode(t, "apply for other, no user",
		callCoded(t, app, fiber.MethodPost, "/v1/events/"+f.ev.ID.String()+"/applications/users/"+uuid.Nil.String(), ""),
		fiber.StatusBadRequest, "user_id_invalid")
}

// The attendee import: an id or a body it cannot read.
func TestAttendeeImportRequestCodes(t *testing.T) {
	t.Parallel()
	f := newTicketCodesFixture(t)
	app := ticketCodesApp(t, f.leader, f.events, f.tickets)
	for _, path := range []string{"/attendee-imports", "/attendee-imports/preview"} {
		got := callCoded(t, app, fiber.MethodPost, "/v1/events/x"+path, `{"text":"a"}`)
		wantCode(t, path+" bad id", got, fiber.StatusBadRequest, "invalid_id")
		if got.body["param"] != "eventId" {
			t.Fatalf("param: %s", got.raw)
		}
		wantCode(t, path+" bad body", callCoded(t, app, fiber.MethodPost, "/v1/events/"+f.ev.ID.String()+path, `{"text":`),
			fiber.StatusBadRequest, "invalid_body")
	}
}

// The forms service's report: what of it core cannot use.
func TestFormResponseCodes(t *testing.T) {
	t.Parallel()
	f := newTicketCodesFixture(t)
	forms := authn.Identity{ID: uuid.New(), Client: "forms", ServiceAccount: true, Product: authz.ProductForms, Roles: []string{"ticket:forms"}}
	app := ticketCodesApp(t, forms, f.events, f.tickets)
	formID := uuid.New().String()
	report := func(form, body string) codedAnswer {
		return callCoded(t, app, fiber.MethodPost, "/v1/forms/"+form+"/responses", body)
	}
	wantCode(t, "no form", report(uuid.Nil.String(), `{"status":"accepted"}`), fiber.StatusBadRequest, "form_id_required")
	wantCode(t, "status", report(formID, `{"status":"maybe"}`), fiber.StatusBadRequest, "form_status_invalid")
	wantCode(t, "user", report(formID, `{"status":"accepted","userId":"`+uuid.Nil.String()+`"}`), fiber.StatusBadRequest, "user_id_invalid")
	wantCode(t, "guest name", report(formID, `{"status":"accepted","guest":{"firstName":"Ada","lastName":"","email":"ada@example.com"}}`),
		fiber.StatusBadRequest, "guest_name_required")
	got := report("x", `{}`)
	wantCode(t, "bad form id", got, fiber.StatusBadRequest, "invalid_id")
	if got.body["param"] != "formId" {
		t.Fatalf("param: %s", got.raw)
	}
	wantCode(t, "bad body", report(formID, `{"status":`), fiber.StatusBadRequest, "invalid_body")
}

// Guest apply's observer answers with a problem even when the error
// handler could not write one.
func TestGuestApplyObserveWritesInternalErrorProblem(t *testing.T) {
	t.Parallel()
	g := NewGuestApply(clientip.Ranges{}, nil, GuestApplyLimits{}, nil)
	app := fiber.New(fiber.Config{ErrorHandler: func(fiber.Ctx, error) error { return errors.New("handler down") }})
	app.Post("/apply", g.Observe, func(fiber.Ctx) error { return errors.New("boom") })
	got := callCoded(t, app, fiber.MethodPost, "/apply", "")
	wantCode(t, "observe", got, fiber.StatusInternalServerError, "internal_error")
	if strings.Contains(got.raw, "boom") || strings.Contains(got.raw, "handler down") {
		t.Fatalf("the cause leaked: %s", got.raw)
	}
}
