package ticket_test

import (
	"bytes"
	"context"
	"errors"
	"log"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
)

type undoFixture struct {
	events  event.Store
	tickets *ticket.MemoryStore
	svc     ticket.Service
	logs    *bytes.Buffer
	ev      event.Event
	day     event.Day
	sess    event.Session
	tk      ticket.Ticket
	staff   authz.Principal
}

func newUndoFixture(t *testing.T, status event.Status) undoFixture {
	t.Helper()
	ctx := context.Background()
	f := undoFixture{events: event.NewMemoryStore(), tickets: ticket.NewMemoryStore(), logs: &bytes.Buffer{}}
	staff := uuid.New()
	f.staff = authz.Principal{ID: staff.String()}
	ev, err := f.events.Create(ctx, event.Event{Name: "Hack", Location: "YTÜ", OwnerTeam: "WEBLAB", Status: status, DoorStaffIDs: []uuid.UUID{staff}})
	if err != nil {
		t.Fatal(err)
	}
	f.ev = ev
	f.day = seedDay(t, f.events, ev.ID, "Day 1")
	f.sess = seedSession(t, f.events, f.day.ID, "Opening")
	f.tk, err = f.tickets.Create(ctx, ticket.Ticket{EventID: ev.ID, TicketType: ticket.Guest, GuestFirstName: "Ada", GuestLastName: "Lovelace", GuestEmail: "ada@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	f.svc = ticket.NewService(f.tickets, f.events, authz.NewAuthorizer(authz.DefaultPolicy()), log.New(f.logs, "", 0))
	return f
}

// The door desk takes back a wrong check-in: the Session's door activity
// drops, a second undo finds nothing (404), and the Ticket can be checked
// in again. One log line names who took back which, by id alone.
func TestService_UndoCheckInByWhoMayTakeCheckIns(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newUndoFixture(t, event.StatusPublished)
	for name, p := range map[string]authz.Principal{"door staff": f.staff, "the team's Leader": weblabLead} {
		t.Run(name, func(t *testing.T) {
			if _, err := f.svc.CheckIn(ctx, p, f.tk.ID, f.sess.ID); err != nil {
				t.Fatal(err)
			}
			before, err := f.svc.DoorActivity(ctx, p, f.sess.ID)
			if err != nil {
				t.Fatal(err)
			}
			if before.Total != 1 || len(before.Items) != 1 {
				t.Fatalf("before %+v", before)
			}
			f.logs.Reset()
			if err := f.svc.UndoCheckIn(ctx, p, f.tk.ID, f.sess.ID); err != nil {
				t.Fatal(err)
			}
			after, err := f.svc.DoorActivity(ctx, p, f.sess.ID)
			if err != nil {
				t.Fatal(err)
			}
			if after.Total != 0 || len(after.Items) != 0 {
				t.Fatalf("after %+v", after)
			}
			line := f.logs.String()
			for _, want := range []string{`"event":"ticket_checkin_undo"`, p.ID, f.tk.ID.String(), f.sess.ID.String(), f.ev.ID.String()} {
				if !strings.Contains(line, want) {
					t.Fatalf("log %q lacks %q", line, want)
				}
			}
			if strings.Contains(line, "ada@example.com") || strings.Contains(line, "Lovelace") || strings.Count(line, "\n") != 1 {
				t.Fatalf("log %q", line)
			}
			if err := f.svc.UndoCheckIn(ctx, p, f.tk.ID, f.sess.ID); !errors.Is(err, ticket.ErrNotFound) {
				t.Fatalf("second undo: %v", err)
			}
		})
	}
}

// Anyone who may not take check-ins at the door may not take one back,
// whether there is one or not; the check-in stays.
func TestService_UndoCheckInForbiddenForWhoMayNotUseTheDoor(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newUndoFixture(t, event.StatusPublished)
	if _, err := f.svc.CheckIn(ctx, weblabLead, f.tk.ID, f.sess.ID); err != nil {
		t.Fatal(err)
	}
	member := authz.Principal{ID: "mem", Groups: []string{"/UYELER/ARGE/WEBLAB"}}
	for _, p := range []authz.Principal{member, otherMember, {ID: "nobody"}} {
		if err := f.svc.UndoCheckIn(ctx, p, f.tk.ID, f.sess.ID); !errors.Is(err, ticket.ErrForbidden) {
			t.Fatalf("%v: %v", p.Groups, err)
		}
	}
	if ok, err := f.tickets.HasCheckIn(ctx, f.tk.ID, f.sess.ID); err != nil || !ok {
		t.Fatalf("check-in gone: %v %v", ok, err)
	}
	if f.logs.Len() != 0 {
		t.Fatalf("logged %q", f.logs.String())
	}
}

// The Ticket and Session are checked as for a check-in: an unknown one is
// 404, a Session of another Event 400, and a Ticket without a check-in at
// the Session 404.
func TestService_UndoCheckInChecksTicketAndSession(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newUndoFixture(t, event.StatusPublished)
	other := seedEvent(t, f.events, "WEBLAB")
	otherSess := seedSession(t, f.events, seedDay(t, f.events, other.ID, "Other").ID, "Other talk")
	second := seedSession(t, f.events, f.day.ID, "Second")
	if _, err := f.svc.CheckIn(ctx, weblabLead, f.tk.ID, f.sess.ID); err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		ticketID, sessionID uuid.UUID
		want                error
	}{
		"unknown ticket":           {uuid.New(), f.sess.ID, ticket.ErrNotFound},
		"unknown session":          {f.tk.ID, uuid.New(), ticket.ErrNotFound},
		"another Event's session":  {f.tk.ID, otherSess.ID, ticket.ErrInvalid},
		"not checked in there yet": {f.tk.ID, second.ID, ticket.ErrNotFound},
	} {
		if err := f.svc.UndoCheckIn(ctx, weblabLead, tc.ticketID, tc.sessionID); !errors.Is(err, tc.want) {
			t.Fatalf("%s: %v, want %v", name, err, tc.want)
		}
	}
	if ok, _ := f.tickets.HasCheckIn(ctx, f.tk.ID, f.sess.ID); !ok {
		t.Fatal("a refused undo removed the check-in")
	}
}

// A door that takes no check-ins takes none back either: a cancelled Event
// answers session_closed, a draft session_closed to who may see it and 404
// to door staff, who may not.
func TestService_UndoCheckInRefusedWhileTheDoorIsClosed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for name, tc := range map[string]struct {
		status event.Status
		p      func(undoFixture) authz.Principal
		want   error
	}{
		"cancelled, Leader":     {event.StatusCancelled, func(undoFixture) authz.Principal { return weblabLead }, ticket.ErrSessionCancelled},
		"cancelled, staff":      {event.StatusCancelled, func(f undoFixture) authz.Principal { return f.staff }, ticket.ErrSessionClosed},
		"draft, Leader":         {event.StatusDraft, func(undoFixture) authz.Principal { return weblabLead }, ticket.ErrSessionNotPublished},
		"draft, door staff":     {event.StatusDraft, func(f undoFixture) authz.Principal { return f.staff }, ticket.ErrNotFound},
		"cancelled, a stranger": {event.StatusCancelled, func(undoFixture) authz.Principal { return otherMember }, ticket.ErrSessionClosed},
	} {
		t.Run(name, func(t *testing.T) {
			f := newUndoFixture(t, tc.status)
			// The check-in from before the door closed.
			if _, err := f.tickets.AddCheckIn(ctx, ticket.CheckIn{TicketID: f.tk.ID, SessionID: f.sess.ID, EventDayID: f.day.ID}); err != nil {
				t.Fatal(err)
			}
			if err := f.svc.UndoCheckIn(ctx, tc.p(f), f.tk.ID, f.sess.ID); !errors.Is(err, tc.want) {
				t.Fatalf("%v, want %v", err, tc.want)
			}
			if ok, _ := f.tickets.HasCheckIn(ctx, f.tk.ID, f.sess.ID); !ok {
				t.Fatal("the check-in was removed")
			}
		})
	}
}

// The settled check-in hook follows check-ins that happened: an undo
// passes through it without firing.
func TestWithSettledCheckInDoesNotFireOnUndo(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newUndoFixture(t, event.StatusPublished)
	fired := 0
	svc := ticket.WithSettledCheckIn(f.svc, func(context.Context, uuid.UUID) { fired++ })
	if _, err := svc.CheckIn(ctx, weblabLead, f.tk.ID, f.sess.ID); err != nil {
		t.Fatal(err)
	}
	if err := svc.UndoCheckIn(ctx, weblabLead, f.tk.ID, f.sess.ID); err != nil {
		t.Fatal(err)
	}
	if fired != 1 {
		t.Fatalf("fired %d times", fired)
	}
	if ok, _ := f.tickets.HasCheckIn(ctx, f.tk.ID, f.sess.ID); ok {
		t.Fatal("the wrapper did not pass the undo through")
	}
}

// The door holds the check-in's own id (door activity, resolve and scan
// answers), not its Ticket's: the undo by that id follows the same rules.
func TestService_UndoCheckInByID(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newUndoFixture(t, event.StatusPublished)
	second := seedSession(t, f.events, f.day.ID, "Second")
	ci, err := f.svc.CheckIn(ctx, f.staff, f.tk.ID, f.sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	elsewhere, err := f.svc.CheckIn(ctx, f.staff, f.tk.ID, second.ID)
	if err != nil {
		t.Fatal(err)
	}
	activity, err := f.svc.DoorActivity(ctx, f.staff, f.sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if activity.Total != 1 || activity.Items[0].ID != ci.ID {
		t.Fatalf("activity %+v", activity)
	}

	member := authz.Principal{ID: "mem", Groups: []string{"/UYELER/ARGE/WEBLAB"}}
	if err := f.svc.UndoCheckInByID(ctx, member, f.sess.ID, ci.ID); !errors.Is(err, ticket.ErrForbidden) {
		t.Fatalf("member: %v", err)
	}
	// Another Session's check-in, or none at all, is not found here.
	if err := f.svc.UndoCheckInByID(ctx, f.staff, f.sess.ID, elsewhere.ID); !errors.Is(err, ticket.ErrNotFound) {
		t.Fatalf("another session's: %v", err)
	}
	if err := f.svc.UndoCheckInByID(ctx, f.staff, f.sess.ID, uuid.New()); !errors.Is(err, ticket.ErrNotFound) {
		t.Fatalf("unknown: %v", err)
	}
	if f.logs.Len() != 0 {
		t.Fatalf("logged %q", f.logs.String())
	}

	if err := f.svc.UndoCheckInByID(ctx, f.staff, f.sess.ID, ci.ID); err != nil {
		t.Fatal(err)
	}
	if activity, err = f.svc.DoorActivity(ctx, f.staff, f.sess.ID); err != nil || activity.Total != 0 {
		t.Fatalf("after %+v %v", activity, err)
	}
	if !strings.Contains(f.logs.String(), `"ticket_checkin_undo"`) || !strings.Contains(f.logs.String(), f.tk.ID.String()) {
		t.Fatalf("log %q", f.logs.String())
	}
	if err := f.svc.UndoCheckInByID(ctx, f.staff, f.sess.ID, ci.ID); !errors.Is(err, ticket.ErrNotFound) {
		t.Fatalf("second undo: %v", err)
	}
	if ok, _ := f.tickets.HasCheckIn(ctx, f.tk.ID, second.ID); !ok {
		t.Fatal("the other Session's check-in was removed")
	}

	// A closed door takes none back by id either.
	closed := newUndoFixture(t, event.StatusCancelled)
	old, err := closed.tickets.AddCheckIn(ctx, ticket.CheckIn{TicketID: closed.tk.ID, SessionID: closed.sess.ID, EventDayID: closed.day.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := closed.svc.UndoCheckInByID(ctx, closed.staff, closed.sess.ID, old.ID); !errors.Is(err, ticket.ErrSessionCancelled) {
		t.Fatalf("cancelled: %v", err)
	}
	if _, err := closed.tickets.CheckInByID(ctx, old.ID); err != nil {
		t.Fatalf("the check-in was removed: %v", err)
	}
}
