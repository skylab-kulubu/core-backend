package ticket_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
)

// A draft's form writes Tickets (ADR 0066): its editors linked the form and
// shared it, and Forms reports an answer once.
func TestRecordFormResponseWritesTicketsOnADraft(t *testing.T) {
	t.Parallel()
	events, svc := setup(t)
	formID := uuid.New()
	ev, err := events.Create(context.Background(), event.Event{
		Name: "Taslak", Location: "YTÜ", OwnerTeam: "WEBLAB", Status: event.StatusDraft, FormURL: formAddress(formID),
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := svc.RecordFormResponse(context.Background(), formsService, acceptedGuest(formID, "Ada", "Lovelace", "ada@example.com"))
	if err != nil || result.TicketsWritten != 1 {
		t.Fatalf("report on a draft: %+v %v", result, err)
	}
	if got := storedGuest(t, svc, ev, "ada@example.com"); got.TicketType != ticket.Guest {
		t.Fatalf("ticket %+v", got)
	}
}
