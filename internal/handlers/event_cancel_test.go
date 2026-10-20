package handlers

import (
	"strings"
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

// Cancelling an Event (ADR 0066, ticket 04): who may, on what, and what a
// cancelled Event still does.

func TestEventCancelWhoAndWhen(t *testing.T) {
	t.Parallel()
	e := newDraftEnv(t)
	pub, draft, other := e.published.ID.String(), e.draft.ID.String(), e.otherDraft.ID.String()
	for _, tc := range []struct {
		name   string
		ident  authn.Identity
		path   string
		body   string
		status int
		code   string
	}{
		{"signed out", draftAnon, pub, "", fiber.StatusUnauthorized, ""},
		{"team member", draftMember, pub, "", fiber.StatusForbidden, ""},
		{"DK reads, does not cancel", draftDK, pub, "", fiber.StatusForbidden, ""},
		{"door staff", draftDoor, pub, "", fiber.StatusForbidden, ""},
		{"draft is deleted, not cancelled", draftEditor, draft, "", fiber.StatusConflict, "event_not_published"},
		{"another team's draft does not exist", draftEditor, other, "", fiber.StatusNotFound, ""},
		{"reason too long", draftEditor, pub, `{"reason":"` + strings.Repeat("ç", event.CancelReasonMax+1) + `"}`, fiber.StatusBadRequest, ""},
		{"malformed body", draftEditor, pub, `{"reason":5}`, fiber.StatusBadRequest, ""},
	} {
		got := draftCall(t, e.app(t, tc.ident), fiber.MethodPost, "/v1/events/"+tc.path+"/cancel", tc.body)
		if code, _ := problemCodeOf(got); got.status != tc.status || code != tc.code {
			t.Errorf("%s: %d %q, want %d %q %s", tc.name, got.status, code, tc.status, tc.code, got.raw)
		}
	}
	// Still published after every refusal.
	if got := draftCall(t, e.app(t, draftAnon), fiber.MethodGet, "/v1/events/"+pub, ""); got.body["status"] != "published" {
		t.Fatalf("after refusals %s", got.raw)
	}

	editor := e.app(t, draftEditor)
	first := draftCall(t, editor, fiber.MethodPost, "/v1/events/"+pub+"/cancel", `{"reason":"  Hava koşulları nedeniyle  ","notifyTicketHolders":true}`)
	if first.status != fiber.StatusOK || first.body["status"] != "cancelled" || first.body["active"] != false ||
		first.body["cancelReason"] != "Hava koşulları nedeniyle" || first.body["cancelledAt"] == nil {
		t.Fatalf("cancel %d %s", first.status, first.raw)
	}
	again := draftCall(t, editor, fiber.MethodPost, "/v1/events/"+pub+"/cancel", "")
	if again.status != fiber.StatusOK || again.body["cancelledAt"] != first.body["cancelledAt"] || again.body["cancelReason"] != first.body["cancelReason"] {
		t.Fatalf("idempotent %d %s", again.status, again.raw)
	}
	// Publish and unpublish refuse a cancelled Event, as before.
	for _, change := range []string{"publish", "unpublish"} {
		if got := draftCall(t, editor, fiber.MethodPost, "/v1/events/"+pub+"/"+change, ""); got.status != fiber.StatusConflict {
			t.Fatalf("%s %d %s", change, got.status, got.raw)
		}
	}

	// An ended Event is not cancelled.
	past := time.Now().Add(-72 * time.Hour)
	ended, err := e.events.Create(t.Context(), event.Event{Name: "Bitti", Location: "YTÜ", OwnerTeam: "WEBLAB", StartDate: &past})
	if err != nil {
		t.Fatal(err)
	}
	if got := draftCall(t, editor, fiber.MethodPost, "/v1/events/"+ended.ID.String()+"/cancel", ""); got.status != fiber.StatusConflict || got.body["code"] != "event_ended" {
		t.Fatalf("ended %d %s", got.status, got.raw)
	}
	// An undated one never ends, so it is cancelled.
	undated, err := e.events.Create(t.Context(), event.Event{Name: "Tarihsiz", Location: "YTÜ", OwnerTeam: "WEBLAB"})
	if err != nil {
		t.Fatal(err)
	}
	if got := draftCall(t, editor, fiber.MethodPost, "/v1/events/"+undated.ID.String()+"/cancel", ""); got.status != fiber.StatusOK || got.body["cancelReason"] != nil {
		t.Fatalf("undated %d %s", got.status, got.raw)
	}
}

// A cancelled Event stays visible; nobody new registers or checks in; the
// door apps leave it out; nothing it held is deleted.
func TestCancelledEventWritesAndReads(t *testing.T) {
	t.Parallel()
	e := newDraftEnv(t)
	ctx := t.Context()
	pub, sess := e.published.ID.String(), e.pubSession.ID.String()
	held, err := e.tickets.Create(ctx, ticket.Ticket{EventID: e.published.ID, TicketType: ticket.Registered, OwnerID: &draftMember.ID})
	if err != nil {
		t.Fatal(err)
	}
	if got := draftCall(t, e.app(t, draftDoor), fiber.MethodPost, "/v1/tickets/"+held.ID.String()+"/sessions/"+sess+"/check-in", ""); got.status != fiber.StatusCreated {
		t.Fatalf("check-in before %d %s", got.status, got.raw)
	}
	before, err := e.tickets.ListByEvent(ctx, e.published.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := draftCall(t, e.app(t, draftEditor), fiber.MethodPost, "/v1/events/"+pub+"/cancel", `{"reason":"Ertelendi"}`); got.status != fiber.StatusOK {
		t.Fatalf("cancel %d %s", got.status, got.raw)
	}

	type want struct {
		status       int
		code, reason string
	}
	registration := want{fiber.StatusConflict, "event_cancelled", ""}
	closed := want{fiber.StatusForbidden, "session_closed", "event_cancelled"}
	other := uuid.MustParse("c1000000-0000-0000-0000-0000000000aa")
	seedTicketUser(t, e.users, other)
	for _, tc := range []struct {
		name, method, path, body string
		ident                    authn.Identity
		want                     want
	}{
		{"apply me", fiber.MethodPost, "/v1/events/" + pub + "/applications/me", "", draftOtherLeader, registration},
		{"apply guest", fiber.MethodPost, "/v1/events/" + pub + "/applications/guest", `{"firstName":"Ada","lastName":"Lovelace","email":"yeni@example.com"}`, draftAnon, registration},
		{"add a person from the panel", fiber.MethodPost, "/v1/events/" + pub + "/applications/users/" + other.String(), "", draftEditor, registration},
		{"check in me", fiber.MethodPost, "/v1/sessions/" + sess + "/check-in/me", "", draftMember, closed},
		{"check in a ticket", fiber.MethodPost, "/v1/tickets/" + held.ID.String() + "/sessions/" + sess + "/check-in", "", draftDoor, closed},
		{"guest check-in", fiber.MethodPost, "/v1/sessions/" + sess + "/check-in/guest", `{"email":"ada@example.com"}`, draftAnon, closed},
		{"door QR", fiber.MethodPost, "/v1/sessions/" + sess + "/door-qr", "", draftEditor, closed},
		{"resolve", fiber.MethodPost, "/v1/sessions/" + sess + "/check-in/resolve", `{"personId":"` + draftMember.ID.String() + `"}`, draftDoor, closed},
		{"read", fiber.MethodGet, "/v1/events/" + pub, "", draftAnon, want{status: fiber.StatusOK}},
		{"door activity", fiber.MethodGet, "/v1/sessions/" + sess + "/check-ins", "", draftDoor, want{status: fiber.StatusOK}},
		{"applicants", fiber.MethodGet, "/v1/events/" + pub + "/tickets", "", draftEditor, want{status: fiber.StatusOK}},
	} {
		got := draftCall(t, e.app(t, tc.ident), tc.method, tc.path, tc.body)
		code, reason := problemCodeOf(got)
		if got.status != tc.want.status || code != tc.want.code || reason != tc.want.reason {
			t.Errorf("%s: %d %q %q, want %+v %s", tc.name, got.status, code, reason, tc.want, got.raw)
		}
	}

	if names := listedNames(t, draftCall(t, e.app(t, draftAnon), fiber.MethodGet, "/v1/events/active", "")); len(names) != 0 {
		t.Errorf("active list %v", names)
	}
	if names := listedNames(t, draftCall(t, e.app(t, draftAnon), fiber.MethodGet, "/v1/events", "")); len(names) != 0 {
		t.Errorf("signed-out list %v", names)
	}
	if names := listedNames(t, draftCall(t, e.app(t, draftMember), fiber.MethodGet, "/v1/events", "")); len(names) != 1 || names[0] != "Yayında" {
		t.Errorf("signed-in list %v", names)
	}
	if names := listedNames(t, draftCall(t, e.app(t, draftDoor), fiber.MethodGet, "/v1/door/events", "")); len(names) != 0 {
		t.Errorf("door events %v", names)
	}

	after, err := e.tickets.ListByEvent(ctx, e.published.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("tickets %d → %d", len(before), len(after))
	}
	if checkedIn, err := e.tickets.HasCheckIn(ctx, held.ID, e.pubSession.ID); err != nil || !checkedIn {
		t.Fatalf("check-in kept %v %v", checkedIn, err)
	}
}

// Forms' report writes no Ticket on a cancelled Event: 200 with outcome
// event_cancelled when every Event of the form is, still a 2xx for Forms.
func TestFormsAnswerToACancelledEventWritesNoTicket(t *testing.T) {
	t.Parallel()
	events, tickets, users := event.NewMemoryStore(), ticket.NewMemoryStore(), user.NewMemoryStore()
	az := authz.NewAuthorizer(authz.DefaultPolicy())
	person := uuid.MustParse("c1000000-0000-0000-0000-0000000000bb")
	seedTicketUser(t, users, person)
	start := time.Now().Add(48 * time.Hour)
	cancelledForm, sharedForm := uuid.New(), uuid.New()
	create := func(name string, form uuid.UUID, cancel bool) event.Event {
		created, err := events.Create(t.Context(), event.Event{
			Name: name, Location: "YTÜ", OwnerTeam: "WEBLAB", StartDate: &start,
			FormURL: "https://forms.yildizskylab.com/" + form.String(),
		})
		if err != nil {
			t.Fatal(err)
		}
		if cancel {
			if _, err := events.Cancel(t.Context(), created.ID, nil, ""); err != nil {
				t.Fatal(err)
			}
		}
		return created
	}
	cancelled := create("İptal", cancelledForm, true)
	sharedCancelled := create("Ortak iptal", sharedForm, true)
	sharedOpen := create("Ortak açık", sharedForm, false)

	th := NewTicketHandler(ticket.NewService(tickets, events, az, users))
	app := fiber.New()
	app.Use(func(c fiber.Ctx) error {
		c.Locals(authn.LocalsIdentity, authn.Identity{ID: uuid.New(), Client: "forms", ServiceAccount: true, Product: authz.ProductForms, Roles: []string{"ticket:forms"}})
		return c.Next()
	})
	app.Post("/v1/forms/:formId/responses", th.RecordFormResponse)
	report := func(form uuid.UUID) (int, map[string]any) {
		return sendEventJSON(t, app, fiber.MethodPost, "/v1/forms/"+form.String()+"/responses",
			`{"responseId":"`+uuid.NewString()+`","status":"accepted","userId":"`+person.String()+`"}`)
	}
	count := func(id uuid.UUID) int {
		listed, err := tickets.ListByEvent(t.Context(), id)
		if err != nil {
			t.Fatal(err)
		}
		return len(listed)
	}
	if status, body := report(cancelledForm); status != fiber.StatusOK || body["outcome"] != "event_cancelled" || count(cancelled.ID) != 0 {
		t.Fatalf("cancelled: %d %v, %d Tickets", status, body, count(cancelled.ID))
	}
	if status, body := report(sharedForm); status != fiber.StatusNoContent || count(sharedOpen.ID) != 1 || count(sharedCancelled.ID) != 0 {
		t.Fatalf("shared: %d %v, open %d cancelled %d", status, body, count(sharedOpen.ID), count(sharedCancelled.ID))
	}
}
