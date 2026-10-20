package handlers

import (
	"fmt"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authn"
)

// Copying an Event: POST /v1/events/{id}/copy (ADR 0066, ticket 06).
func TestEventCopyEndpoint(t *testing.T) {
	t.Parallel()
	e := newDraftEnv(t)
	pub, draft, other := e.published.ID.String(), e.draft.ID.String(), e.otherDraft.ID.String()
	body := func(id uuid.UUID, extra string) string {
		return fmt.Sprintf(`{"id":%q,"name":"Kopya","startDate":"2027-03-14T19:00:00+03:00"%s}`, id, extra)
	}
	for _, tc := range []struct {
		name   string
		ident  authn.Identity
		path   string
		body   string
		status int
	}{
		{"signed out", draftAnon, pub, body(uuid.New(), ""), fiber.StatusUnauthorized},
		{"team member", draftMember, pub, body(uuid.New(), ""), fiber.StatusForbidden},
		{"DK reads, does not copy", draftDK, draft, body(uuid.New(), ""), fiber.StatusForbidden},
		{"another team's draft does not exist", draftEditor, other, body(uuid.New(), ""), fiber.StatusNotFound},
		{"no id", draftEditor, pub, `{"name":"Kopya","startDate":"2027-03-14T19:00:00+03:00"}`, fiber.StatusBadRequest},
		{"no name", draftEditor, pub, fmt.Sprintf(`{"id":%q,"startDate":"2027-03-14T19:00:00+03:00"}`, uuid.New()), fiber.StatusBadRequest},
		{"no start", draftEditor, pub, fmt.Sprintf(`{"id":%q,"name":"Kopya"}`, uuid.New()), fiber.StatusBadRequest},
		{"malformed", draftEditor, pub, `{"id":5}`, fiber.StatusBadRequest},
		{"bad source id", draftEditor, "x", body(uuid.New(), ""), fiber.StatusBadRequest},
		{"the id is another Event's", draftEditor, pub, body(e.draft.ID, ""), fiber.StatusConflict},
	} {
		if got := draftCall(t, e.app(t, tc.ident), fiber.MethodPost, "/v1/events/"+tc.path+"/copy", tc.body); got.status != tc.status {
			t.Errorf("%s: %d, want %d %s", tc.name, got.status, tc.status, got.raw)
		}
	}

	editor := e.app(t, draftEditor)
	id := uuid.New()
	first := draftCall(t, editor, fiber.MethodPost, "/v1/events/"+pub+"/copy", body(id, `,"includeDoorStaff":true,"includeCompetitions":true`))
	copied, _ := first.body["event"].(map[string]any)
	skipped, _ := first.body["skipped"].([]any)
	if first.status != fiber.StatusCreated || copied["id"] != id.String() || copied["status"] != "draft" ||
		copied["copiedFromEventId"] != pub || len(skipped) != 1 || skipped[0] != "doorStaff" {
		t.Fatalf("copy %d %s", first.status, first.raw)
	}
	again := draftCall(t, editor, fiber.MethodPost, "/v1/events/"+pub+"/copy", body(id, ""))
	if again.status != fiber.StatusOK || again.body["event"].(map[string]any)["id"] != id.String() {
		t.Fatalf("again %d %s", again.status, again.raw)
	}
	// The copy is a draft: nobody else sees it.
	if got := draftCall(t, e.app(t, draftMember), fiber.MethodGet, "/v1/events/"+id.String(), ""); got.status != fiber.StatusNotFound {
		t.Fatalf("member reads the copy: %d", got.status)
	}
}

// Into another Owner team (ticket 16): both teams' rights.
func TestEventCopyIntoAnotherTeam(t *testing.T) {
	t.Parallel()
	e := newDraftEnv(t)
	pub := e.published.ID.String()
	body := func(id uuid.UUID) string {
		return fmt.Sprintf(`{"id":%q,"name":"Kopya","startDate":"2027-03-14T19:00:00+03:00","ownerTeam":"ARTLAB"}`, id)
	}
	both := authn.Identity{ID: uuid.New(), Groups: []string{"/UYELER/ARGE/WEBLAB/LIDERLER", "/UYELER/ARGE/ARTLAB/LIDERLER"}}
	for _, tc := range []struct {
		name   string
		ident  authn.Identity
		status int
	}{
		{"WEBLAB Leader, no right in ARTLAB", draftEditor, fiber.StatusForbidden},
		{"ARTLAB Leader, reads WEBLAB's Event only", draftOtherLeader, fiber.StatusForbidden},
		{"DK", draftDK, fiber.StatusForbidden},
		{"Leader of both", both, fiber.StatusCreated},
	} {
		got := draftCall(t, e.app(t, tc.ident), fiber.MethodPost, "/v1/events/"+pub+"/copy", body(uuid.New()))
		if got.status != tc.status {
			t.Errorf("%s: %d, want %d %s", tc.name, got.status, tc.status, got.raw)
			continue
		}
		if got.status == fiber.StatusCreated && got.body["event"].(map[string]any)["ownerTeam"] != "ARTLAB" {
			t.Errorf("%s: %s", tc.name, got.raw)
		}
	}
}
