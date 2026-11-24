package ticket

import (
	"context"
	"errors"
	"regexp"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/ticketqr"
)

// The Event's door (docs/venue-entry.md): Venue entry without picking a
// Session, the only door of an Event that has none.

// EventClosedError refuses the Event's door; Reason is event_not_published,
// event_cancelled, event_archived or event_ended. It is ErrEventClosed.
type EventClosedError struct{ Reason string }

func (e *EventClosedError) Error() string { return "ticket: event closed: " + e.Reason }

// Unwrap makes it ErrEventClosed.
func (e *EventClosedError) Unwrap() error { return ErrEventClosed }

// eventDoor is the Event eventID whose door p is about to use, asked
// before any code is read: ErrNotFound for none (or a draft p may not
// see); to write (write), an *EventClosedError for a draft, a cancelled or
// archived Event or one ended over a day ago (the Ticket QR's own window),
// then ErrForbidden unless p may take check-ins at its door (canUseDoor);
// to read, who may read its Tickets reads too (DK).
func (s *service) eventDoor(ctx context.Context, p authz.Principal, eventID uuid.UUID, write bool) (event.Event, error) {
	ev, err := s.eventOf(ctx, eventID)
	if err != nil {
		return event.Event{}, err
	}
	if err := s.seen(&p, ev); err != nil {
		return event.Event{}, err
	}
	if !write {
		if s.canUseDoor(ctx, p, ev) ||
			s.authz.Allow(p, authz.Resource{Type: authz.TypeTicket, OwnerTeam: ev.OwnerTeam}, authz.Read) {
			return ev, nil
		}
		return event.Event{}, ErrForbidden
	}
	// A scan sent later is judged at the moment it was taken
	// (docs/offline-door.md).
	if reason := closedReason(ev, s.scanTime(ctx)); reason != "" {
		return event.Event{}, &EventClosedError{Reason: reason}
	}
	if !s.canUseDoor(ctx, p, ev) {
		return event.Event{}, ErrForbidden
	}
	return ev, nil
}

// closedReason is why ev's door takes no entry at now, "" when it is open.
func closedReason(ev event.Event, now time.Time) string {
	switch {
	case ev.Status == event.StatusDraft:
		return "event_not_published"
	case ev.Status == event.StatusCancelled:
		return "event_cancelled"
	case ev.ArchivedAt != nil:
		return "event_archived"
	}
	if end := ev.EndsAt(); end != nil && !now.Before(end.Add(ticketqr.AfterEnd)) {
		return "event_ended"
	}
	return ""
}

func (s *service) AuthorizeEventDoor(ctx context.Context, p authz.Principal, eventID uuid.UUID) error {
	_, err := s.eventDoor(ctx, p, eventID, true)
	return err
}

// EnterTicketQR writes today's Venue entry of the Ticket a Ticket QR names
// (source scan). The Ticket QR's refusals are CheckInTicketQR's.
func (s *service) EnterTicketQR(ctx context.Context, p authz.Principal, eventID uuid.UUID, token string) (DoorEntry, error) {
	entry, err := s.enterTicketQR(ctx, p, eventID, token)
	s.scans.record(err)
	return entry, err
}

func (s *service) enterTicketQR(ctx context.Context, p authz.Principal, eventID uuid.UUID, token string) (DoorEntry, error) {
	ev, err := s.eventDoor(ctx, p, eventID, true)
	if err != nil {
		return DoorEntry{}, err
	}
	t, err := s.ticketOfQR(ctx, ev, token)
	if err != nil {
		return DoorEntry{}, err
	}
	return s.enterEvent(ctx, ev, t, staffEntry(EntryScan, p))
}

// EnterUser writes today's Venue entry of userID's Ticket (a SkyPass,
// Wallet code or card the door scanned; source scan).
func (s *service) EnterUser(ctx context.Context, p authz.Principal, eventID, userID uuid.UUID) (DoorEntry, error) {
	ev, err := s.eventDoor(ctx, p, eventID, true)
	if err != nil {
		return DoorEntry{}, err
	}
	t, err := s.tickets.GetByOwnerEvent(ctx, userID, ev.ID)
	if err != nil {
		return DoorEntry{}, err
	}
	return s.enterEvent(ctx, ev, t, staffEntry(EntryScan, p))
}

// ResolveEntry writes today's Venue entry of the one Ticket the desk's
// target names (source desk), as the Session desk finds it.
func (s *service) ResolveEntry(ctx context.Context, p authz.Principal, eventID uuid.UUID, target DoorCheckInTarget) (DoorEntry, error) {
	ev, err := s.eventDoor(ctx, p, eventID, true)
	if err != nil {
		return DoorEntry{}, err
	}
	t, name, err := s.findDoorTicket(ctx, ev, target)
	if err != nil {
		return DoorEntry{}, err
	}
	entry, err := s.enterEvent(ctx, ev, t, staffEntry(EntryDesk, p))
	if err == nil && entry.Name == "" {
		entry.Name = name
	}
	return entry, err
}

// enterEvent writes t's Venue entry for the venue day now falls on
// (entryDay: the EventDay whose window holds now, a short Event's start
// day, else today). Only an active Ticket enters; a second entry the same
// day is an *EntryConflictError naming who is in.
func (s *service) enterEvent(ctx context.Context, ev event.Event, t Ticket, how entryBy) (DoorEntry, error) {
	if refused := inactiveErr(t); refused != nil {
		return DoorEntry{}, refused
	}
	date, dayID, err := s.eventDay(ctx, ev, s.scanTime(ctx))
	if err != nil {
		return DoorEntry{}, err
	}
	scannedAt, late := s.scanned(ctx)
	e := VenueEntry{TicketID: t.ID, EventID: ev.ID, EntryDate: date, EventDayID: dayID, Source: how.source, ByUserID: how.by,
		ScannedAt: scannedAt, NoMail: late}
	name, _ := s.doorTicketIdentity(ctx, t)
	written, err := s.tickets.AddVenueEntry(ctx, e)
	var conflict *EntryConflictError
	if errors.As(err, &conflict) {
		return DoorEntry{}, &EntryConflictError{Name: name}
	}
	if err != nil {
		return DoorEntry{}, err
	}
	s.scans.recordEntry(written.Source)
	s.logger.Printf(`{"event":"venue_entry","actorId":%q,"eventId":%q,"source":%q}`,
		actorOf(how), ev.ID, written.Source)
	return DoorEntry{
		ID: written.ID, Kind: KindEntry, TicketID: t.ID, EntryDate: written.EntryDate,
		EventDayID: written.EventDayID, Source: written.Source, Name: name, CreatedAt: written.CreatedAt,
		ScannedAt: written.ScannedAt,
	}, nil
}

// actorOf is the door staff's id for a log line, "" for nobody.
func actorOf(how entryBy) string {
	if how.by == nil {
		return ""
	}
	return how.by.String()
}

// UndoEntry takes back a Venue entry of the Event at its desk: the day's
// Check-ins stay (attendance does not depend on the door). ErrNotFound for
// an entry of another Event.
func (s *service) UndoEntry(ctx context.Context, p authz.Principal, eventID, entryID uuid.UUID) error {
	ev, err := s.eventDoor(ctx, p, eventID, true)
	if err != nil {
		return err
	}
	entry, err := s.tickets.VenueEntryByID(ctx, entryID)
	if err != nil {
		return err
	}
	if entry.EventID != ev.ID {
		return ErrNotFound
	}
	removed, err := s.tickets.RemoveVenueEntry(ctx, entryID)
	if err != nil {
		return err
	}
	if !removed {
		return ErrNotFound
	}
	s.logger.Printf(`{"event":"venue_entry_undo","actorId":%q,"eventId":%q,"entryId":%q}`, p.ID, ev.ID, entryID)
	return nil
}

// EntrySummary counts the Event's Venue entries by day and source.
func (s *service) EntrySummary(ctx context.Context, p authz.Principal, eventID uuid.UUID) (EntrySummary, error) {
	ev, err := s.eventDoor(ctx, p, eventID, false)
	if err != nil {
		return EntrySummary{}, err
	}
	days, err := s.tickets.VenueEntryDays(ctx, ev.ID)
	if err != nil {
		return EntrySummary{}, err
	}
	return EntrySummary{Days: days}, nil
}

var entryDatePattern = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

// ListEntries is the Event's Venue entries on date (YYYY-MM-DD; the
// venue day now falls on when empty, as an entry written now gets),
// newest first: name, time, source, no e-mail. The door's rule comes
// before the date: ErrInvalid for a date that is not one.
func (s *service) ListEntries(ctx context.Context, p authz.Principal, eventID uuid.UUID, date string) ([]DoorEntry, error) {
	ev, err := s.eventDoor(ctx, p, eventID, false)
	if err != nil {
		return nil, err
	}
	if date == "" {
		if date, _, err = s.eventDay(ctx, ev, s.now()); err != nil {
			return nil, err
		}
	}
	if !entryDatePattern.MatchString(date) {
		return nil, ErrInvalid
	}
	if _, err := time.Parse(time.DateOnly, date); err != nil {
		return nil, ErrInvalid
	}
	return s.namedEntries(ctx, ev.ID, date)
}

// now is the service's time.
func (s *service) now() time.Time {
	if s.clock != nil {
		return s.clock()
	}
	return time.Now()
}
