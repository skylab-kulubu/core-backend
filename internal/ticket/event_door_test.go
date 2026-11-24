package ticket_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
)

// An Event with no Session and no day: its door writes today's entry by
// the Event, a second the same day is refused naming who is in; the
// summary and the day's list count it; the desk takes it back.
func TestEventDoorWithoutSessions(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	d := newVenueDoor(t, nil)
	ev, err := d.events.Create(ctx, event.Event{Name: "Buluşma", Location: "YTÜ", OwnerTeam: "WEBLAB", DoorStaffIDs: []uuid.UUID{uuid.MustParse(d.staff.ID)}})
	if err != nil {
		t.Fatal(err)
	}
	owner := uuid.New()
	member, err := d.tickets.Create(ctx, ticket.Ticket{EventID: ev.ID, TicketType: ticket.Registered, OwnerID: &owner})
	if err != nil {
		t.Fatal(err)
	}
	guest, err := d.tickets.Create(ctx, ticket.Ticket{EventID: ev.ID, TicketType: ticket.Guest, GuestFirstName: "Ada", GuestLastName: "Lovelace", GuestEmail: "ada@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	today := ticket.IstanbulDate(time.Now())
	in, err := d.svc.EnterUser(ctx, d.staff, ev.ID, owner)
	if err != nil {
		t.Fatal(err)
	}
	if in.Kind != ticket.KindEntry || in.EntryDate != today || in.Source != ticket.EntryScan || in.EventDayID != nil || in.TicketID != member.ID {
		t.Fatalf("entry %+v", in)
	}
	_, err = d.svc.EnterUser(ctx, d.staff, ev.ID, owner)
	var again *ticket.EntryConflictError
	if !errors.As(err, &again) || !errors.Is(err, ticket.ErrConflict) {
		t.Fatalf("second: %v", err)
	}
	// The desk by the Ticket's code, then by e-mail: one entry a day.
	byCode, err := d.svc.ResolveEntry(ctx, weblabLead, ev.ID, ticket.DoorCheckInTarget{Query: guest.Code})
	if err != nil {
		t.Fatal(err)
	}
	if byCode.Source != ticket.EntryDesk || byCode.Name != "Ada Lovelace" {
		t.Fatalf("desk %+v", byCode)
	}
	_, err = d.svc.ResolveEntry(ctx, weblabLead, ev.ID, ticket.DoorCheckInTarget{Query: "ada@example.com"})
	if !errors.As(err, &again) || again.Name != "Ada Lovelace" {
		t.Fatalf("desk again: %v", err)
	}
	summary, err := d.svc.EntrySummary(ctx, d.staff, ev.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(summary.Days) != 1 || summary.Days[0].Date != today || summary.Days[0].Total != 2 ||
		summary.Days[0].BySource["scan"] != 1 || summary.Days[0].BySource["desk"] != 1 || summary.Days[0].BySource["attendance"] != 0 {
		t.Fatalf("summary %+v", summary)
	}
	list, err := d.svc.ListEntries(ctx, d.staff, ev.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("list %+v", list)
	}
	if _, err := d.svc.ListEntries(ctx, d.staff, ev.ID, "12.12.2026"); !errors.Is(err, ticket.ErrInvalid) {
		t.Fatalf("bad date: %v", err)
	}
	d.logs.Reset()
	if err := d.svc.UndoEntry(ctx, d.staff, ev.ID, byCode.ID); err != nil {
		t.Fatal(err)
	}
	if err := d.svc.UndoEntry(ctx, d.staff, ev.ID, byCode.ID); !errors.Is(err, ticket.ErrNotFound) {
		t.Fatalf("undo again: %v", err)
	}
	if err := d.svc.UndoEntry(ctx, d.staff, d.ev.ID, in.ID); !errors.Is(err, ticket.ErrNotFound) {
		t.Fatalf("another Event's entry: %v", err)
	}
	if line := d.logs.String(); !strings.Contains(line, `"event":"venue_entry_undo"`) || strings.Contains(line, "Lovelace") {
		t.Fatalf("log %q", line)
	}
}

// The desk's undo of an entry leaves the day's Check-ins: attendance does
// not depend on the door.
func TestUndoEntryKeepsTheDaysCheckIns(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	d := newVenueDoor(t, nil)
	if _, err := d.svc.CheckInMe(ctx, authz.Principal{ID: d.memberID.String()}, d.open.ID, ""); err != nil {
		t.Fatal(err)
	}
	got := d.entries(t, firstDay)
	if len(got) != 1 {
		t.Fatalf("entries %+v", got)
	}
	if err := d.svc.UndoEntry(ctx, d.staff, d.ev.ID, got[0].ID); err != nil {
		t.Fatal(err)
	}
	if ok, err := d.tickets.HasCheckIn(ctx, d.member.ID, d.open.ID); err != nil || !ok {
		t.Fatalf("check-in gone: %v %v", ok, err)
	}
}

// Who may use the Event's door: door staff and the team's Leader write; DK
// reads but does not write; another member neither. A draft its caller
// cannot see is 404.
func TestEventDoorAuthorization(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	d := newVenueDoor(t, nil)
	dk := authz.Principal{ID: "dk", Groups: []string{"/UYELER/DK"}}
	member := authz.Principal{ID: "mem", Groups: []string{"/UYELER/ARGE/WEBLAB"}}
	for name, tc := range map[string]struct {
		p           authz.Principal
		write, read error
	}{
		"door staff":        {d.staff, nil, nil},
		"the team's Leader": {weblabLead, nil, nil},
		"DK":                {dk, ticket.ErrForbidden, nil},
		"a team member":     {member, ticket.ErrForbidden, ticket.ErrForbidden},
		"another member":    {otherMember, ticket.ErrForbidden, ticket.ErrForbidden},
	} {
		if err := d.svc.AuthorizeEventDoor(ctx, tc.p, d.ev.ID); !errors.Is(err, tc.write) && !(tc.write == nil && err == nil) {
			t.Fatalf("%s write: %v", name, err)
		}
		if _, err := d.svc.EntrySummary(ctx, tc.p, d.ev.ID); !errors.Is(err, tc.read) && !(tc.read == nil && err == nil) {
			t.Fatalf("%s summary: %v", name, err)
		}
		if _, err := d.svc.ListEntries(ctx, tc.p, d.ev.ID, firstDay); !errors.Is(err, tc.read) && !(tc.read == nil && err == nil) {
			t.Fatalf("%s list: %v", name, err)
		}
	}
	if err := d.svc.AuthorizeEventDoor(ctx, d.staff, uuid.New()); !errors.Is(err, ticket.ErrNotFound) {
		t.Fatalf("no such Event: %v", err)
	}
}

// A draft, a cancelled Event and one ended over a day ago take no entry:
// event_closed with the reason, before the door's own rule.
func TestEventDoorClosed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	d := newVenueDoor(t, nil)
	past := time.Now().Add(-49 * time.Hour)
	for reason, ev := range map[string]event.Event{
		"event_not_published": {Name: "Taslak", Location: "YTÜ", OwnerTeam: "WEBLAB", Status: event.StatusDraft},
		"event_ended":         {Name: "Bitti", Location: "YTÜ", OwnerTeam: "WEBLAB", StartDate: &past, EndDate: &past},
	} {
		created, err := d.events.Create(ctx, ev)
		if err != nil {
			t.Fatal(err)
		}
		err = d.svc.AuthorizeEventDoor(ctx, weblabLead, created.ID)
		var closed *ticket.EventClosedError
		if !errors.As(err, &closed) || closed.Reason != reason || !errors.Is(err, ticket.ErrEventClosed) {
			t.Fatalf("%s: %v", reason, err)
		}
		if reason == "event_not_published" {
			if err := d.svc.AuthorizeEventDoor(ctx, otherMember, created.ID); !errors.Is(err, ticket.ErrNotFound) {
				t.Fatalf("a draft unseen: %v", err)
			}
		}
	}
}
