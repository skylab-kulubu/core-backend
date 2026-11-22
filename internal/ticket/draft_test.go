package ticket_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
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

// An operator adds a guest to a draft from the panel, as a person (Yusuf,
// 2026-11-22, K5); a guest's own application stays refused there, and who
// may not see the draft does not find it.
func TestApplyGuestOnADraftFromThePanel(t *testing.T) {
	t.Parallel()
	events, svc := setup(t)
	ctx := context.Background()
	ev, err := events.Create(ctx, event.Event{Name: "Taslak", Location: "YTÜ", OwnerTeam: "WEBLAB", Status: event.StatusDraft})
	if err != nil {
		t.Fatal(err)
	}
	guest := ticket.GuestInfo{FirstName: "Ada", LastName: "Lovelace", Email: "ada@example.com"}
	applied, err := svc.ApplyGuest(ctx, weblabLead, ev.ID, guest)
	if err != nil || applied.Result != ticket.GuestCreated || !applied.Trusted {
		t.Fatalf("operator: %+v %v", applied, err)
	}
	if got := storedGuest(t, svc, ev, "ada@example.com"); got.TicketType != ticket.Guest {
		t.Fatalf("ticket %+v", got)
	}
	grace := ticket.GuestInfo{FirstName: "Grace", LastName: "Hopper", Email: "grace@example.com"}
	if _, err := svc.ApplyGuest(ctx, authz.Principal{}, ev.ID, grace); !errors.Is(err, ticket.ErrNotFound) {
		t.Fatalf("signed out: %v", err)
	}
	if _, err := svc.ApplyGuest(ctx, formsService, ev.ID, grace); !errors.Is(err, ticket.ErrNotFound) {
		t.Fatalf("a product's service account: %v", err)
	}
}
