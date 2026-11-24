package ticket_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// testClock is a settable ticket.Clock.
type testClock struct {
	mu sync.Mutex
	at time.Time
}

func (c *testClock) set(at time.Time) { c.mu.Lock(); c.at = at; c.mu.Unlock() }
func (c *testClock) now() time.Time   { c.mu.Lock(); defer c.mu.Unlock(); return c.at }

var istanbulZone = func() *time.Location {
	loc, err := time.LoadLocation("Europe/Istanbul")
	if err != nil {
		return time.FixedZone("TRT", 3*60*60)
	}
	return loc
}()

// night is 12 December 2026 at hour (Istanbul), hours past 24 into the
// next morning.
func night(hour int) time.Time {
	return time.Date(2026, 12, 12, 0, 0, 0, 0, istanbulZone).Add(time.Duration(hour) * time.Hour)
}

type nightDoor struct {
	events  *event.MemoryStore
	tickets *ticket.MemoryStore
	svc     ticket.Service
	clock   *testClock
	ev      event.Event
	staff   authz.Principal
}

// newNightDoor is an Event from 19:00 to 07:00 the next morning
// (GECEKODU), with that night as its EventDay when withDay.
func newNightDoor(t *testing.T, withDay bool) (*nightDoor, *event.Day) {
	t.Helper()
	ctx := context.Background()
	d := &nightDoor{events: event.NewMemoryStore(), tickets: ticket.NewMemoryStore(), clock: &testClock{at: night(18)}}
	staff := uuid.New()
	d.staff = authz.Principal{ID: staff.String()}
	start, end := night(19), night(31)
	ev, err := d.events.Create(ctx, event.Event{Name: "Gecekodu", Location: "YTÜ", OwnerTeam: "WEBLAB",
		StartDate: &start, EndDate: &end, DoorStaffIDs: []uuid.UUID{staff}})
	if err != nil {
		t.Fatal(err)
	}
	d.ev = ev
	var day *event.Day
	if withDay {
		created, err := d.events.CreateDay(ctx, event.Day{EventID: ev.ID, Name: "Gece", StartDate: &start, EndDate: &end})
		if err != nil {
			t.Fatal(err)
		}
		day = &created
	}
	d.svc = ticket.NewService(d.tickets, d.events, authz.NewAuthorizer(authz.DefaultPolicy()), user.NewMemoryStore(), ticket.Clock(d.clock.now))
	return d, day
}

func (d *nightDoor) member(t *testing.T) uuid.UUID {
	t.Helper()
	owner := uuid.New()
	if _, err := d.tickets.Create(context.Background(), ticket.Ticket{EventID: d.ev.ID, TicketType: ticket.Registered, OwnerID: &owner}); err != nil {
		t.Fatal(err)
	}
	return owner
}

// A night past midnight is one venue day: scanned at 20:00 and again at
// 01:00 is one entry (the second 409), on the EventDay's start day.
func TestNightEventIsOneVenueDay(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for _, withDay := range []bool{true, false} {
		d, day := newNightDoor(t, withDay)
		owner := d.member(t)
		d.clock.set(night(20))
		in, err := d.svc.EnterUser(ctx, d.staff, d.ev.ID, owner)
		if err != nil {
			t.Fatalf("day %v, 20:00: %v", withDay, err)
		}
		if in.EntryDate != "2026-12-12" {
			t.Fatalf("day %v: date %q", withDay, in.EntryDate)
		}
		if withDay && (in.EventDayID == nil || *in.EventDayID != day.ID) {
			t.Fatalf("EventDay %v", in.EventDayID)
		}
		d.clock.set(night(25))
		if _, err := d.svc.EnterUser(ctx, d.staff, d.ev.ID, owner); !errors.Is(err, ticket.ErrConflict) {
			t.Fatalf("day %v, 01:00: %v", withDay, err)
		}
		// The day's list at 01:00 is the night's.
		list, err := d.svc.ListEntries(ctx, d.staff, d.ev.ID, "")
		if err != nil || len(list) != 1 {
			t.Fatalf("day %v, list at 01:00: %+v %v", withDay, list, err)
		}
		summary, err := d.svc.EntrySummary(ctx, d.staff, d.ev.ID)
		if err != nil || len(summary.Days) != 1 || summary.Days[0].Date != "2026-12-12" {
			t.Fatalf("day %v, summary %+v %v", withDay, summary, err)
		}
	}
}

// In at the door at 01:00, at the 02:00 Session's attendance QR at 02:00:
// one entry, the night's.
func TestNightDoorThenSessionIsOneEntry(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	d, day := newNightDoor(t, true)
	at := night(26)
	sess, err := d.events.CreateSession(ctx, event.Session{EventDayID: day.ID, Title: "Gece yarısı", SpeakerName: "Ada", SessionType: "PRESENTATION", StartTime: &at})
	if err != nil {
		t.Fatal(err)
	}
	owner := d.member(t)
	d.clock.set(night(25))
	if _, err := d.svc.EnterUser(ctx, d.staff, d.ev.ID, owner); err != nil {
		t.Fatal(err)
	}
	d.clock.set(at)
	if _, err := d.svc.CheckInMe(ctx, authz.Principal{ID: owner.String()}, sess.ID, ""); err != nil {
		t.Fatal(err)
	}
	if got, _ := d.tickets.VenueEntriesOn(ctx, d.ev.ID, "2026-12-12"); len(got) != 1 || got[0].Source != ticket.EntryScan {
		t.Fatalf("night %+v", got)
	}
	if got, _ := d.tickets.VenueEntriesOn(ctx, d.ev.ID, "2026-12-13"); len(got) != 0 {
		t.Fatalf("morning %+v", got)
	}
}

// The door's rule comes before the date: someone who may not read the
// door is refused, whatever date they send.
func TestListEntriesAsksTheDoorFirst(t *testing.T) {
	t.Parallel()
	d, _ := newNightDoor(t, false)
	if _, err := d.svc.ListEntries(context.Background(), otherMember, d.ev.ID, "bugün"); !errors.Is(err, ticket.ErrForbidden) {
		t.Fatalf("stranger, bad date: %v", err)
	}
	if _, err := d.svc.ListEntries(context.Background(), d.staff, d.ev.ID, "bugün"); !errors.Is(err, ticket.ErrInvalid) {
		t.Fatalf("staff, bad date: %v", err)
	}
}

// dayDoor is an Event from start to end (end nil: none entered) with the
// EventDays given as start/end pairs, and a member's Ticket.
func dayDoor(t *testing.T, start time.Time, end *time.Time, days ...[2]time.Time) (*nightDoor, []event.Day, uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	d := &nightDoor{events: event.NewMemoryStore(), tickets: ticket.NewMemoryStore(), clock: &testClock{at: start}}
	staff := uuid.New()
	d.staff = authz.Principal{ID: staff.String()}
	ev, err := d.events.Create(ctx, event.Event{Name: "Gecekodu", Location: "YTÜ", OwnerTeam: "WEBLAB",
		StartDate: &start, EndDate: end, DoorStaffIDs: []uuid.UUID{staff}})
	if err != nil {
		t.Fatal(err)
	}
	d.ev = ev
	created := make([]event.Day, 0, len(days))
	for i, window := range days {
		from, to := window[0], window[1]
		day, err := d.events.CreateDay(ctx, event.Day{EventID: ev.ID, Name: fmt.Sprintf("%d. gece", i+1), StartDate: &from, EndDate: &to})
		if err != nil {
			t.Fatal(err)
		}
		created = append(created, day)
	}
	d.svc = ticket.NewService(d.tickets, d.events, authz.NewAuthorizer(authz.DefaultPolicy()), user.NewMemoryStore(), ticket.Clock(d.clock.now))
	return d, created, d.member(t)
}

// An Event with no end entered (it lasts 24 hours by its dates) and one of
// exactly 24 hours (19:00 → 19:00), with no EventDay: 20:00 and the next
// 01:00 are one entry, on the start day; the second is 409.
func TestDaylessEventOfADayIsOneVenueDay(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	whole := night(43)
	for name, end := range map[string]*time.Time{"no end": nil, "exactly 24 hours": &whole} {
		d, _, owner := dayDoor(t, night(19), end)
		d.clock.set(night(20))
		in, err := d.svc.EnterUser(ctx, d.staff, d.ev.ID, owner)
		if err != nil {
			t.Fatalf("%s, 20:00: %v", name, err)
		}
		if in.EntryDate != "2026-12-12" || in.EventDayID != nil {
			t.Fatalf("%s: %+v", name, in)
		}
		d.clock.set(night(25))
		if _, err := d.svc.EnterUser(ctx, d.staff, d.ev.ID, owner); !errors.Is(err, ticket.ErrConflict) {
			t.Fatalf("%s, 01:00: %v", name, err)
		}
	}
}

// An Event of two nights (12 Dec 19:00 → 13 Dec 07:00, 13 Dec 19:00 →
// 14 Dec 07:00): 01:00 on 14 Dec is the second night's (13 Dec, EventDay
// 2). A scan in the gap between the two nights (13 Dec 12:00) is that
// calendar day with no EventDay, as today.
func TestMultiDayEventNights(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	end := night(55)
	d, days, owner := dayDoor(t, night(19), &end,
		[2]time.Time{night(19), night(31)}, [2]time.Time{night(43), night(55)})
	second := d.member(t)

	d.clock.set(night(49)) // 14 Dec 01:00
	in, err := d.svc.EnterUser(ctx, d.staff, d.ev.ID, owner)
	if err != nil {
		t.Fatal(err)
	}
	if in.EntryDate != "2026-12-13" || in.EventDayID == nil || *in.EventDayID != days[1].ID {
		t.Fatalf("second night: %+v", in)
	}

	d.clock.set(night(36)) // 13 Dec 12:00, between the nights
	gap, err := d.svc.EnterUser(ctx, d.staff, d.ev.ID, second)
	if err != nil {
		t.Fatal(err)
	}
	if gap.EntryDate != "2026-12-13" || gap.EventDayID != nil {
		t.Fatalf("gap: %+v", gap)
	}
}
