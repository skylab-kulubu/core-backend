package ticket_test

import (
	"context"
	"io"
	"log"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/identity"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
)

// A Walk-in whose address an account holds verified is written linked
// (docs/guest-account-links.md): GUEST with what the person gave, owned by
// the account, found at the door by the e-mail as before.
func TestWalkInIsWrittenLinked(t *testing.T) {
	t.Parallel()
	d := newWalkInDoor(t, event.Event{})
	owner := uuid.New()
	dir := identity.NewMemory()
	dir.PutGroup(d.weblab)
	owners := &fakeGuestOwners{owners: map[string]uuid.UUID{"ayse@example.com": owner}}
	d.svc = ticket.NewService(mailingStore{d.tickets}, d.events, authz.NewAuthorizer(authz.DefaultPolicy()), d.users, dir, d.audit,
		log.New(io.Discard, "", 0), ticket.GuestOwners(owners))
	out, err := d.walkIn(d.staff, ayse)
	if err != nil || out.Result != ticket.WalkInCreated {
		t.Fatalf("%+v %v", out, err)
	}
	got, err := d.tickets.GetByGuestEvent(context.Background(), "ayse@example.com", d.ev.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.TicketType != ticket.Guest || got.OwnerID == nil || *got.OwnerID != owner || got.GuestUniversity == "" {
		t.Fatalf("stored %+v", got)
	}
	found, err := d.svc.SearchDoorAttendees(context.Background(), d.staff, d.ev.ID, "ayse@example.com")
	if err != nil || len(found) != 1 || found[0].Email != "ayse@example.com" {
		t.Fatalf("door search: %+v %v", found, err)
	}
}
