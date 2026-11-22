package handlers

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/event"
)

// publishRecorder is a memory store that remembers what each publication
// asked of the draft's Tickets' Notifications.
type publishRecorder struct {
	*event.MemoryStore
	mu    sync.Mutex
	asked map[uuid.UUID]bool
}

func (r *publishRecorder) Publish(ctx context.Context, id uuid.UUID, by *uuid.UUID, notifyTickets bool) (bool, error) {
	r.mu.Lock()
	r.asked[id] = notifyTickets
	r.mu.Unlock()
	return r.MemoryStore.Publish(ctx, id, by, notifyTickets)
}

// POST /v1/events/{id}/publish takes notifyTickets from the body's
// notifyTicketHolders (K3, 2026-11-22): no body, or a body without it, asks
// for no mail (sky-app); a malformed body is 400.
func TestPublishPassesNotifyTicketHolders(t *testing.T) {
	t.Parallel()
	store := &publishRecorder{MemoryStore: event.NewMemoryStore(), asked: map[uuid.UUID]bool{}}
	app := eventApp(t, weblabLeader(), store)
	start := time.Now().Add(48 * time.Hour)
	draft := func() event.Event {
		e, err := store.Create(t.Context(), event.Event{Name: "Taslak", Location: "YTÜ", OwnerTeam: "WEBLAB", Status: event.StatusDraft, StartDate: &start})
		if err != nil {
			t.Fatal(err)
		}
		return e
	}
	for _, tc := range []struct {
		name, body string
		want       bool
	}{
		{"asked", `{"notifyTicketHolders":true}`, true},
		{"not asked", `{"notifyTicketHolders":false}`, false},
		{"no field", `{}`, false},
		{"no body", ``, false},
	} {
		ev := draft()
		status, body := sendEventJSON(t, app, fiber.MethodPost, "/v1/events/"+ev.ID.String()+"/publish", tc.body)
		if status != fiber.StatusOK || body["status"] != "published" {
			t.Fatalf("%s: %d %v", tc.name, status, body)
		}
		store.mu.Lock()
		asked, called := store.asked[ev.ID]
		store.mu.Unlock()
		if !called || asked != tc.want {
			t.Errorf("%s: notifyTickets %v (called %v), want %v", tc.name, asked, called, tc.want)
		}
	}
	ev := draft()
	if status, body := sendEventJSON(t, app, fiber.MethodPost, "/v1/events/"+ev.ID.String()+"/publish", `{"notifyTicketHolders":"evet"}`); status != fiber.StatusBadRequest {
		t.Fatalf("malformed body: %d %v", status, body)
	}
}
