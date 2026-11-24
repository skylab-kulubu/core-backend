package ticket_test

import (
	"bytes"
	"context"
	"errors"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// venueDoor is an Event with two dated days, a Session on each (and a
// second on the first), a guest and a member Ticket, and its door staff.
type venueDoor struct {
	events           *event.MemoryStore
	tickets          *ticket.MemoryStore
	svc              ticket.Service
	logs             *bytes.Buffer
	scans            *ticket.DoorScanMetrics
	ev               event.Event
	day1, day2       event.Day
	open, talk, next event.Session
	guest, member    ticket.Ticket
	memberID         uuid.UUID
	staff            authz.Principal
}

func newVenueDoor(t *testing.T, mode *event.EntryMode) *venueDoor {
	t.Helper()
	ctx := context.Background()
	d := &venueDoor{events: event.NewMemoryStore(), tickets: ticket.NewMemoryStore(), logs: &bytes.Buffer{}, scans: &ticket.DoorScanMetrics{}}
	users := user.NewMemoryStore()
	staff := uuid.New()
	d.staff = authz.Principal{ID: staff.String()}
	end := time.Now().Add(72 * time.Hour)
	ev, err := d.events.Create(ctx, event.Event{
		Name: "Hack", Location: "YTÜ", OwnerTeam: "WEBLAB", EndDate: &end, DoorStaffIDs: []uuid.UUID{staff}, EntryMode: mode,
	})
	if err != nil {
		t.Fatal(err)
	}
	first := time.Date(2026, 12, 12, 7, 0, 0, 0, time.UTC)
	second := first.Add(24 * time.Hour)
	if d.day1, err = d.events.CreateDay(ctx, event.Day{EventID: ev.ID, Name: "1. gün", StartDate: &first}); err != nil {
		t.Fatal(err)
	}
	if d.day2, err = d.events.CreateDay(ctx, event.Day{EventID: ev.ID, Name: "2. gün", StartDate: &second}); err != nil {
		t.Fatal(err)
	}
	d.open = seedSession(t, d.events, d.day1.ID, "Açılış")
	d.talk = seedSession(t, d.events, d.day1.ID, "Konuşma")
	d.next = seedSession(t, d.events, d.day2.ID, "Final")
	if d.ev, err = d.events.Get(ctx, ev.ID); err != nil {
		t.Fatal(err)
	}
	if d.guest, err = d.tickets.Create(ctx, ticket.Ticket{
		EventID: ev.ID, TicketType: ticket.Guest, GuestFirstName: "Ada", GuestLastName: "Lovelace", GuestEmail: "ada@example.com",
	}); err != nil {
		t.Fatal(err)
	}
	d.memberID = uuid.New()
	if _, _, err := user.NewService(users).Ensure(ctx, d.memberID, user.Profile{Email: "grace@example.com", FirstName: "Grace", LastName: "Hopper"}); err != nil {
		t.Fatal(err)
	}
	if d.member, err = d.tickets.Create(ctx, ticket.Ticket{EventID: ev.ID, TicketType: ticket.Registered, OwnerID: &d.memberID}); err != nil {
		t.Fatal(err)
	}
	d.svc = ticket.NewService(d.tickets, d.events, authz.NewAuthorizer(authz.DefaultPolicy()), users, d.scans, log.New(d.logs, "", 0))
	return d
}

// entries are the Event's entries on date, by source.
func (d *venueDoor) entries(t *testing.T, date string) []ticket.DoorEntry {
	t.Helper()
	got, err := d.tickets.VenueEntriesOn(context.Background(), d.ev.ID, date)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func entryMode(m event.EntryMode) *event.EntryMode { return &m }

const (
	firstDay  = "2026-12-12"
	secondDay = "2026-12-13"
)

// per_session (automatic: the Event has Sessions): a door scan checks the
// person in and writes the day's entry (scan) once; the next Session that
// day adds a Check-in, not another entry; a second scan at the same
// Session is 409.
func TestPerSessionScanWritesCheckInAndTheDaysEntry(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	d := newVenueDoor(t, nil)
	if d.ev.EffectiveEntryMode != event.EntryPerSession {
		t.Fatalf("mode %q", d.ev.EffectiveEntryMode)
	}
	ci, err := d.svc.CheckInUser(ctx, d.staff, d.open.ID, d.memberID)
	if err != nil {
		t.Fatal(err)
	}
	if ci.Kind != ticket.KindCheckIn {
		t.Fatalf("kind %q", ci.Kind)
	}
	if _, err := d.svc.CheckInUser(ctx, d.staff, d.talk.ID, d.memberID); err != nil {
		t.Fatal(err)
	}
	if _, err := d.svc.CheckInUser(ctx, d.staff, d.open.ID, d.memberID); !errors.Is(err, ticket.ErrConflict) {
		t.Fatalf("second scan: %v", err)
	}
	got := d.entries(t, firstDay)
	if len(got) != 1 || got[0].Source != ticket.EntryScan || got[0].TicketID != d.member.ID || *got[0].EventDayID != d.day1.ID {
		t.Fatalf("entries %+v", got)
	}
	if text := d.scans.Prometheus(); !strings.Contains(text, `skylab_venue_entries_total{source="scan"} 1`) {
		t.Fatalf("metrics\n%s", text)
	}
}

// venue (chosen, the Event has Sessions): a scan at a Session's door
// writes the day's entry alone, kind entry; a second scan that day is 409
// at any of its Sessions; the next day's Session writes that day's.
func TestVenueScanWritesTheDaysEntryAlone(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	d := newVenueDoor(t, entryMode(event.EntryVenue))
	if d.ev.EffectiveEntryMode != event.EntryVenue {
		t.Fatalf("mode %q", d.ev.EffectiveEntryMode)
	}
	ci, err := d.svc.CheckInUser(ctx, d.staff, d.open.ID, d.memberID)
	if err != nil {
		t.Fatal(err)
	}
	if ci.Kind != ticket.KindEntry || ci.SessionID != d.open.ID || ci.TicketID != d.member.ID {
		t.Fatalf("answer %+v", ci)
	}
	if ok, _ := d.tickets.HasCheckIn(ctx, d.member.ID, d.open.ID); ok {
		t.Fatal("a check-in was written")
	}
	if _, err := d.svc.CheckInUser(ctx, d.staff, d.talk.ID, d.memberID); !errors.Is(err, ticket.ErrConflict) {
		t.Fatalf("second scan that day: %v", err)
	}
	if _, err := d.svc.CheckInUser(ctx, d.staff, d.next.ID, d.memberID); err != nil {
		t.Fatalf("next day: %v", err)
	}
	if len(d.entries(t, firstDay)) != 1 || len(d.entries(t, secondDay)) != 1 {
		t.Fatalf("entries %+v %+v", d.entries(t, firstDay), d.entries(t, secondDay))
	}
	// The Session's door activity lists the day's entries.
	activity, err := d.svc.DoorActivity(ctx, d.staff, d.open.ID)
	if err != nil {
		t.Fatal(err)
	}
	if activity.Total != 1 || len(activity.Items) != 1 || activity.Items[0].Kind != ticket.KindEntry ||
		activity.Items[0].PersonName != "Grace Hopper" || activity.Items[0].ID != ci.ID {
		t.Fatalf("activity %+v", activity)
	}
	// The door's undo by id takes the entry back.
	if err := d.svc.UndoCheckInByID(ctx, d.staff, d.open.ID, ci.ID); err != nil {
		t.Fatal(err)
	}
	if len(d.entries(t, firstDay)) != 0 {
		t.Fatal("entry kept")
	}
}

// Every other Session Check-in (the person's own, a guest's, the desk's)
// writes the day's entry when there is none, of its own source, and
// leaves one already there alone. Nobody is refused for not being scanned
// at the door.
func TestCheckInWritesTheMissingEntry(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	d := newVenueDoor(t, entryMode(event.EntryVenue))
	if _, err := d.svc.CheckInMe(ctx, authz.Principal{ID: d.memberID.String()}, d.open.ID, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := d.svc.CheckInGuest(ctx, d.open.ID, ticket.GuestCheckIn{Email: "ada@example.com"}); err != nil {
		t.Fatal(err)
	}
	got := d.entries(t, firstDay)
	if len(got) != 2 || got[0].Source != ticket.EntryAttendance || got[1].Source != ticket.EntryAttendance {
		t.Fatalf("entries %+v", got)
	}
	// The desk on the second day.
	if _, err := d.svc.ResolveAndCheckIn(ctx, d.staff, d.next.ID, ticket.DoorCheckInTarget{Query: "ada@example.com"}); err != nil {
		t.Fatal(err)
	}
	if got := d.entries(t, secondDay); len(got) != 1 || got[0].Source != ticket.EntryDesk {
		t.Fatalf("desk %+v", got)
	}
	// The next Session that day leaves the entry as it was.
	if _, err := d.svc.CheckInMe(ctx, authz.Principal{ID: d.memberID.String()}, d.talk.ID, ""); err != nil {
		t.Fatal(err)
	}
	if got := d.entries(t, firstDay); len(got) != 2 {
		t.Fatalf("entries %+v", got)
	}
}

// A cancelled or waitlisted Ticket gets neither a Check-in nor an entry,
// in either mode.
func TestInactiveTicketNeitherChecksInNorEnters(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for _, mode := range []*event.EntryMode{nil, entryMode(event.EntryVenue)} {
		d := newVenueDoor(t, mode)
		for status, want := range map[ticket.Status]error{
			ticket.StatusCancelled:  ticket.ErrTicketCancelled,
			ticket.StatusWaitlisted: ticket.ErrTicketWaitlisted,
		} {
			owner := uuid.New()
			tk, err := d.tickets.Create(ctx, ticket.Ticket{EventID: d.ev.ID, TicketType: ticket.Registered, OwnerID: &owner, Status: status})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := d.svc.CheckInUser(ctx, d.staff, d.open.ID, owner); !errors.Is(err, want) {
				t.Fatalf("%v scan: %v", status, err)
			}
			if _, err := d.svc.CheckIn(ctx, d.staff, tk.ID, d.open.ID); !errors.Is(err, want) {
				t.Fatalf("%v desk: %v", status, err)
			}
			if _, err := d.svc.EnterUser(ctx, d.staff, d.ev.ID, owner); !errors.Is(err, want) {
				t.Fatalf("%v event door: %v", status, err)
			}
		}
		if got := d.entries(t, firstDay); len(got) != 0 {
			t.Fatalf("entries %+v", got)
		}
	}
}

// Undoing a Check-in takes back the entry it wrote, unless the Ticket has
// another Check-in that day (which holds it then, and takes it back when
// undone itself); a scanned entry stays.
func TestUndoCheckInTakesBackTheEntryItWrote(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	d := newVenueDoor(t, nil)
	me := authz.Principal{ID: d.memberID.String()}
	if _, err := d.svc.CheckInMe(ctx, me, d.open.ID, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := d.svc.CheckInMe(ctx, me, d.talk.ID, ""); err != nil {
		t.Fatal(err)
	}
	if err := d.svc.UndoCheckIn(ctx, d.staff, d.member.ID, d.open.ID); err != nil {
		t.Fatal(err)
	}
	if got := d.entries(t, firstDay); len(got) != 1 {
		t.Fatalf("with another Check-in that day: %+v", got)
	}
	if err := d.svc.UndoCheckIn(ctx, d.staff, d.member.ID, d.talk.ID); err != nil {
		t.Fatal(err)
	}
	if got := d.entries(t, firstDay); len(got) != 0 {
		t.Fatalf("after the last Check-in: %+v", got)
	}
	// A scan's entry stays when its Check-in goes.
	if _, err := d.svc.CheckInUser(ctx, d.staff, d.open.ID, d.memberID); err != nil {
		t.Fatal(err)
	}
	if err := d.svc.UndoCheckIn(ctx, d.staff, d.member.ID, d.open.ID); err != nil {
		t.Fatal(err)
	}
	if got := d.entries(t, firstDay); len(got) != 1 || got[0].Source != ticket.EntryScan {
		t.Fatalf("scanned entry: %+v", got)
	}
}

// An Event with no live Session works in the venue mode, whatever was
// chosen: its door is the Event's.
func TestEventWithoutSessionsIsVenue(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	events := event.NewMemoryStore()
	for _, mode := range []*event.EntryMode{nil, entryMode(event.EntryPerSession), entryMode(event.EntryVenue)} {
		ev, err := events.Create(ctx, event.Event{Name: "Buluşma", Location: "YTÜ", OwnerTeam: "WEBLAB", EntryMode: mode})
		if err != nil {
			t.Fatal(err)
		}
		if ev.EffectiveEntryMode != event.EntryVenue {
			t.Fatalf("%v: %q", mode, ev.EffectiveEntryMode)
		}
	}
}
