package ticket_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
)

// An ended Event takes no new application from its form (ADR 0069,
// addendum of 2026-11-22). A form an ended Event and a current one both
// list (shared before the one-place rule) writes the current one's Ticket
// alone; when every Event left has ended, nothing is written. An Event
// without dates never ends.
func TestRecordFormResponseWritesNoTicketOnAnEndedEvent(t *testing.T) {
	t.Parallel()
	events, svc := setup(t)
	ctx := context.Background()
	ago, soon := time.Now().Add(-72*time.Hour), time.Now().Add(48*time.Hour)
	create := func(name string, start *time.Time, formID uuid.UUID) event.Event {
		ev, err := events.Create(ctx, event.Event{Name: name, Location: "YTÜ", OwnerTeam: "WEBLAB", Status: event.StatusPublished, StartDate: start, FormURL: formAddress(formID)})
		if err != nil {
			t.Fatal(err)
		}
		return ev
	}
	shared, endedOnly, undated := uuid.New(), uuid.New(), uuid.New()
	old := create("Geçen yıl", &ago, shared)
	current := create("Bu yıl", &soon, shared)
	lone := create("Bitti", &ago, endedOnly)
	noDates := create("Tarihsiz", nil, undated)

	result, err := svc.RecordFormResponse(ctx, formsService, acceptedGuest(shared, "Ada", "Lovelace", "ada@example.com"))
	if err != nil || result.Outcome != ticket.FormResponseRecorded || result.TicketsWritten != 1 || result.ListingEvents != 2 {
		t.Fatalf("shared: %+v %v", result, err)
	}
	if len(ticketsOf(t, svc, old)) != 0 || len(ticketsOf(t, svc, current)) != 1 {
		t.Fatalf("shared: old %d current %d", len(ticketsOf(t, svc, old)), len(ticketsOf(t, svc, current)))
	}

	result, err = svc.RecordFormResponse(ctx, formsService, acceptedGuest(endedOnly, "Ada", "Lovelace", "ada@example.com"))
	if err != nil || result.Outcome != ticket.FormResponseEventEnded || result.TicketsWritten != 0 || result.ListingEvents != 1 {
		t.Fatalf("ended: %+v %v", result, err)
	}
	if listed := ticketsOf(t, svc, lone); len(listed) != 0 {
		t.Fatalf("ended Event got %+v", listed)
	}

	result, err = svc.RecordFormResponse(ctx, formsService, acceptedGuest(undated, "Ada", "Lovelace", "ada@example.com"))
	if err != nil || result.Outcome != ticket.FormResponseRecorded || len(ticketsOf(t, svc, noDates)) != 1 {
		t.Fatalf("undated: %+v %v", result, err)
	}
}
