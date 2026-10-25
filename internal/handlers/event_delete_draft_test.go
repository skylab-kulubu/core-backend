package handlers

import (
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/skylab-kulubu/core-backend/internal/event"
)

// DELETE /v1/events/{id}: a Leader deletes a draft, gets 409 event_not_draft
// on a published Event; YK (event:manage) archives it (ticket 05).
func TestEventDeleteOnlyDraftHTTP(t *testing.T) {
	t.Parallel()
	store := event.NewMemoryStore()
	draft, err := store.Create(t.Context(), event.Event{Name: "Taslak", Location: "YTÜ", OwnerTeam: "WEBLAB", Status: event.StatusDraft})
	if err != nil {
		t.Fatal(err)
	}
	published, err := store.Create(t.Context(), event.Event{Name: "Yayında", Location: "YTÜ", OwnerTeam: "WEBLAB"})
	if err != nil {
		t.Fatal(err)
	}
	leader, manager := eventApp(t, weblabLeader(), store), eventApp(t, yk(), store)
	if status, body := sendEventJSON(t, leader, fiber.MethodDelete, "/v1/events/"+published.ID.String(), ""); status != fiber.StatusConflict || body["code"] != "event_not_draft" {
		t.Fatalf("leader on published: %d %v", status, body)
	}
	if status, body := sendEventJSON(t, leader, fiber.MethodGet, "/v1/events/"+published.ID.String(), ""); status != fiber.StatusOK {
		t.Fatalf("published after refused delete: %d %v", status, body)
	}
	requireStatus(t, leader, fiber.MethodDelete, "/v1/events/"+draft.ID.String(), fiber.StatusNoContent)
	requireStatus(t, leader, fiber.MethodPost, "/v1/events/"+draft.ID.String()+"/restore", fiber.StatusOK)
	requireStatus(t, manager, fiber.MethodDelete, "/v1/events/"+published.ID.String(), fiber.StatusNoContent)
	requireStatus(t, leader, fiber.MethodGet, "/v1/events/"+published.ID.String(), fiber.StatusNotFound)
}
