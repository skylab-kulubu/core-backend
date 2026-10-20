package handlers

import (
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/skylab-kulubu/core-backend/internal/authn"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
)

// Undoing a cancellation (ADR 0066 addendum 2026-10-19, ticket 15): always
// possible, back to published, everything as before.
func TestEventUncancel(t *testing.T) {
	t.Parallel()
	e := newDraftEnv(t)
	ctx := t.Context()
	pub, draft, sess := e.published.ID.String(), e.draft.ID.String(), e.pubSession.ID.String()
	editor := e.app(t, draftEditor)
	held, err := e.tickets.Create(ctx, ticket.Ticket{EventID: e.published.ID, TicketType: ticket.Registered, OwnerID: &draftMember.ID})
	if err != nil {
		t.Fatal(err)
	}
	before := draftCall(t, editor, fiber.MethodGet, "/v1/events/"+pub, "")
	if got := draftCall(t, editor, fiber.MethodPost, "/v1/events/"+pub+"/cancel", `{"reason":"Ertelendi","notifyTicketHolders":true}`); got.status != fiber.StatusOK {
		t.Fatalf("cancel %d %s", got.status, got.raw)
	}

	for _, tc := range []struct {
		name   string
		ident  authn.Identity
		path   string
		status int
		code   string
	}{
		{"signed out", draftAnon, pub, fiber.StatusUnauthorized, ""},
		{"team member", draftMember, pub, fiber.StatusForbidden, ""},
		{"DK", draftDK, pub, fiber.StatusForbidden, ""},
		{"a draft is not cancelled", draftEditor, draft, fiber.StatusConflict, "event_not_cancelled"},
		{"another team's draft does not exist", draftEditor, e.otherDraft.ID.String(), fiber.StatusNotFound, ""},
	} {
		got := draftCall(t, e.app(t, tc.ident), fiber.MethodPost, "/v1/events/"+tc.path+"/uncancel", "")
		if code, _ := problemCodeOf(got); got.status != tc.status || code != tc.code {
			t.Errorf("%s: %d %q, want %d %q %s", tc.name, got.status, code, tc.status, tc.code, got.raw)
		}
	}

	back := draftCall(t, editor, fiber.MethodPost, "/v1/events/"+pub+"/uncancel", `{"notifyTicketHolders":true}`)
	if back.status != fiber.StatusOK || back.body["status"] != "published" || back.body["active"] != true ||
		back.body["cancelledAt"] != nil || back.body["cancelReason"] != nil || back.body["publishedAt"] != before.body["publishedAt"] {
		t.Fatalf("uncancel %d %s", back.status, back.raw)
	}
	if again := draftCall(t, editor, fiber.MethodPost, "/v1/events/"+pub+"/uncancel", ""); again.status != fiber.StatusOK || again.body["status"] != "published" {
		t.Fatalf("idempotent %d %s", again.status, again.raw)
	}

	// Every path works as before the cancellation, and what it held is
	// there.
	for _, tc := range []struct {
		name, path, body string
		ident            authn.Identity
		status           int
	}{
		{"apply me", "/v1/events/" + pub + "/applications/me", "", draftOtherLeader, fiber.StatusCreated},
		{"apply guest", "/v1/events/" + pub + "/applications/guest", `{"firstName":"Ada","lastName":"Lovelace","email":"geri@example.com"}`, draftAnon, fiber.StatusCreated},
		{"check in a ticket", "/v1/tickets/" + held.ID.String() + "/sessions/" + sess + "/check-in", "", draftDoor, fiber.StatusCreated},
	} {
		if got := draftCall(t, e.app(t, tc.ident), fiber.MethodPost, tc.path, tc.body); got.status != tc.status {
			t.Errorf("%s: %d %s", tc.name, got.status, got.raw)
		}
	}
	if names := listedNames(t, draftCall(t, e.app(t, draftDoor), fiber.MethodGet, "/v1/door/events", "")); len(names) != 1 {
		t.Errorf("door events %v", names)
	}
	if names := listedNames(t, draftCall(t, e.app(t, draftAnon), fiber.MethodGet, "/v1/events/active", "")); len(names) != 1 {
		t.Errorf("active list %v", names)
	}

	// An ended Event is uncancelled too: published, not active.
	start := time.Now().Add(-2 * time.Hour)
	running, err := e.events.Create(ctx, event.Event{Name: "Sürüyor", Location: "YTÜ", OwnerTeam: "WEBLAB", StartDate: &start})
	if err != nil {
		t.Fatal(err)
	}
	if got := draftCall(t, editor, fiber.MethodPost, "/v1/events/"+running.ID.String()+"/cancel", ""); got.status != fiber.StatusOK {
		t.Fatalf("cancel running %d %s", got.status, got.raw)
	}
	ended := time.Now().Add(-72 * time.Hour)
	moved := running
	moved.StartDate = &ended
	if _, err := e.events.Update(ctx, moved); err != nil {
		t.Fatal(err)
	}
	if got := draftCall(t, editor, fiber.MethodPost, "/v1/events/"+running.ID.String()+"/uncancel", ""); got.status != fiber.StatusOK ||
		got.body["status"] != "published" || got.body["active"] != false {
		t.Fatalf("uncancel ended %d %s", got.status, got.raw)
	}
}
