package ticket_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
)

// Venue entry on the database (docs/venue-entry.md): a Check-in writes the
// day's entry in the same statement, naming itself and the door staff (an
// active account; a principal with no account row names nobody and still
// writes); a scan's entry names no Check-in and outlives it; undoing the
// last Check-in of the day takes the derived entry back, an earlier one
// hands it on; one entry a Ticket a day; a cancelled Ticket none; the day's
// list names the person; the summary counts by source.
func TestVenueEntriesOnPostgres(t *testing.T) {
	f := startFormResponsePostgres(t)
	ctx := context.Background()
	store := ticket.NewPostgresStore(f.pool)
	svc := f.service(store)
	staffID := f.person(t, "kapi@example.com")
	staff := authz.Principal{ID: staffID.String()}
	start := time.Date(2026, 12, 12, 7, 0, 0, 0, time.UTC)
	end := time.Now().Add(72 * time.Hour)
	ev, err := f.events.Create(ctx, event.Event{Name: "Hack", Location: "YTÜ", OwnerTeam: "WEBLAB", EndDate: &end, DoorStaffIDs: []uuid.UUID{staffID}})
	if err != nil {
		t.Fatal(err)
	}
	day, err := f.events.CreateDay(ctx, event.Day{EventID: ev.ID, Name: "1. gün", StartDate: &start})
	if err != nil {
		t.Fatal(err)
	}
	open := seedSession(t, f.events, day.ID, "Açılış")
	talk := seedSession(t, f.events, day.ID, "Konuşma")
	if got, err := f.events.Get(ctx, ev.ID); err != nil || got.EffectiveEntryMode != event.EntryPerSession {
		t.Fatalf("mode %q %v", got.EffectiveEntryMode, err)
	}
	guest, err := store.Create(ctx, ticket.Ticket{EventID: ev.ID, TicketType: ticket.Guest, GuestFirstName: "Ada", GuestLastName: "Lovelace", GuestEmail: "ada@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	memberID := f.person(t, "grace@example.com")
	member, err := store.Create(ctx, ticket.Ticket{EventID: ev.ID, TicketType: ticket.Registered, OwnerID: &memberID})
	if err != nil {
		t.Fatal(err)
	}
	type row struct {
		source    string
		checkInID *uuid.UUID
		by        *uuid.UUID
	}
	read := func(ticketID uuid.UUID) []row {
		t.Helper()
		rows, err := f.pool.Query(ctx, `SELECT source, check_in_id, by_user_id FROM venue_entries WHERE ticket_id = $1`, ticketID)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []row
		for rows.Next() {
			var r row
			if err := rows.Scan(&r.source, &r.checkInID, &r.by); err != nil {
				t.Fatal(err)
			}
			out = append(out, r)
		}
		return out
	}

	// The desk's Check-in: its entry names it and the door staff.
	desk, err := svc.CheckIn(ctx, staff, guest.ID, open.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := read(guest.ID); len(got) != 1 || got[0].source != "desk" || got[0].checkInID == nil || *got[0].checkInID != desk.ID ||
		got[0].by == nil || *got[0].by != staffID {
		t.Fatalf("desk entry %+v", got)
	}
	// The next Session that day: no second entry; undoing the first hands
	// the entry to the second, undoing that takes it back.
	if _, err := svc.CheckIn(ctx, weblabLead, guest.ID, talk.ID); err != nil {
		t.Fatal(err)
	}
	if err := svc.UndoCheckIn(ctx, staff, guest.ID, open.ID); err != nil {
		t.Fatal(err)
	}
	if got := read(guest.ID); len(got) != 1 || got[0].checkInID == nil || *got[0].checkInID == desk.ID {
		t.Fatalf("handed on %+v", got)
	}
	if err := svc.UndoCheckIn(ctx, staff, guest.ID, talk.ID); err != nil {
		t.Fatal(err)
	}
	if got := read(guest.ID); len(got) != 0 {
		t.Fatalf("after the last Check-in %+v", got)
	}

	// A scan by a principal with no account row: the entry names nobody.
	ghost := authz.Principal{ID: uuid.NewString(), Groups: []string{"/UYELER/ARGE/WEBLAB/LIDERLER"}}
	if _, err := svc.CheckInUser(ctx, ghost, open.ID, memberID); err != nil {
		t.Fatal(err)
	}
	if got := read(member.ID); len(got) != 1 || got[0].source != "scan" || got[0].checkInID != nil || got[0].by != nil {
		t.Fatalf("scan entry %+v", got)
	}
	if err := svc.UndoCheckIn(ctx, staff, member.ID, open.ID); err != nil {
		t.Fatal(err)
	}
	if got := read(member.ID); len(got) != 1 {
		t.Fatalf("scan entry after undo %+v", got)
	}
	// One a day; a cancelled Ticket none.
	_, err = store.AddVenueEntry(ctx, ticket.VenueEntry{TicketID: member.ID, EntryDate: "2026-12-12", Source: ticket.EntryDesk})
	if !errors.Is(err, ticket.ErrConflict) {
		t.Fatalf("second entry: %v", err)
	}
	if _, err := f.pool.Exec(ctx, `UPDATE tickets SET status = 'cancelled', cancelled_at = now(), cancelled_by = 'operator' WHERE id = $1`, guest.ID); err != nil {
		t.Fatal(err)
	}
	_, err = store.AddVenueEntry(ctx, ticket.VenueEntry{TicketID: guest.ID, EntryDate: "2026-12-12", Source: ticket.EntryDesk})
	if !errors.Is(err, ticket.ErrTicketCancelled) {
		t.Fatalf("cancelled: %v", err)
	}
	if _, err := svc.CheckIn(ctx, staff, guest.ID, open.ID); !errors.Is(err, ticket.ErrTicketCancelled) {
		t.Fatalf("cancelled check-in: %v", err)
	}

	// The day's list and the summary.
	list, err := svc.ListEntries(ctx, staff, ev.ID, "2026-12-12")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Name != "Ada Lovelace" || list[0].Kind != ticket.KindEntry || list[0].EventDayID == nil || *list[0].EventDayID != day.ID {
		t.Fatalf("list %+v", list)
	}
	summary, err := svc.EntrySummary(ctx, staff, ev.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(summary.Days) != 1 || summary.Days[0].Total != 1 || summary.Days[0].BySource["scan"] != 1 || summary.Days[0].EventDayID == nil {
		t.Fatalf("summary %+v", summary)
	}

	// The venue mode: the Session's door activity lists the day's entry,
	// and its undo by id takes it back.
	if _, err := f.pool.Exec(ctx, `UPDATE events SET entry_mode = 'venue' WHERE id = $1`, ev.ID); err != nil {
		t.Fatal(err)
	}
	activity, err := svc.DoorActivity(ctx, staff, open.ID)
	if err != nil {
		t.Fatal(err)
	}
	if activity.Total != 1 || len(activity.Items) != 1 || activity.Items[0].Kind != ticket.KindEntry || activity.Items[0].PersonName != "Ada Lovelace" {
		t.Fatalf("activity %+v", activity)
	}
	if err := svc.UndoCheckInByID(ctx, staff, open.ID, activity.Items[0].ID); err != nil {
		t.Fatal(err)
	}
	if got := read(member.ID); len(got) != 0 {
		t.Fatalf("after undo by id %+v", got)
	}
	// The Event's door writes today's, on no EventDay (none is today).
	in, err := svc.EnterUser(ctx, staff, ev.ID, memberID)
	if err != nil {
		t.Fatal(err)
	}
	if in.EntryDate != ticket.IstanbulDate(time.Now()) || in.EventDayID != nil || in.Name != "Ada Lovelace" {
		t.Fatalf("event door %+v", in)
	}
}
