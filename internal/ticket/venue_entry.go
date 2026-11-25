package ticket

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/event"
)

// Venue entry (docs/venue-entry.md): a Ticket entering the venue on a day,
// one a day. It is never counted for a certificate.

// EntrySource is how a Venue entry was written.
type EntrySource string

const (
	// EntryScan: the door scanned a credential (Ticket QR, SkyPass,
	// Wallet, card).
	EntryScan EntrySource = "scan"
	// EntryDesk: the door desk found the person (name, e-mail, person,
	// Ticket code) or checked them in by their Ticket.
	EntryDesk EntrySource = "desk"
	// EntryAttendance: a Session Check-in (attendance QR, check-in/me, a
	// guest's own) wrote it, the person not scanned at the door that day.
	EntryAttendance EntrySource = "attendance"
)

// entrySources are the sources in the order the metric and the summary
// list them.
var entrySources = []EntrySource{EntryScan, EntryDesk, EntryAttendance}

// Kinds of what a door answer names: a Session Check-in or a Venue entry.
const (
	KindCheckIn = "check_in"
	KindEntry   = "entry"
)

// ErrEventClosed refuses the Event's door: a draft (with
// event.ErrNotPublished), cancelled (event.ErrCancelled), archived or
// ended a day ago (event.ErrEnded). 403 event_closed.
var ErrEventClosed = errors.New("ticket: event closed")

// VenueEntry is a Ticket's entry into the venue on EntryDate, the
// Europe/Istanbul day (YYYY-MM-DD).
type VenueEntry struct {
	ID         uuid.UUID   `json:"id"`
	TicketID   uuid.UUID   `json:"-"`
	EventID    uuid.UUID   `json:"eventId"`
	EntryDate  string      `json:"entryDate"`
	EventDayID *uuid.UUID  `json:"eventDayId,omitempty"`
	Source     EntrySource `json:"source"`
	// CheckInID is the Check-in that wrote it (source attendance or desk
	// at a Session); undoing that Check-in takes it back.
	CheckInID *uuid.UUID `json:"-"`
	// ByUserID is the door staff who wrote it, when an active account.
	ByUserID  *uuid.UUID `json:"-"`
	CreatedAt time.Time  `json:"createdAt"`
	// ScannedAt is when the door took the scan, when it sent it later
	// (docs/offline-door.md); nil for a scan core took as it came.
	ScannedAt *time.Time `json:"scannedAt,omitempty"`
	// NoMail queues no door mail with it: a scan sent too late for one.
	NoMail bool `json:"-"`
}

// DoorEntry is a Venue entry as the door shows it: the person's name, no
// Ticket id and no e-mail (as the door's other answers).
type DoorEntry struct {
	ID         uuid.UUID   `json:"id"`
	Kind       string      `json:"kind"`
	TicketID   uuid.UUID   `json:"-"`
	EntryDate  string      `json:"entryDate"`
	EventDayID *uuid.UUID  `json:"eventDayId,omitempty"`
	Source     EntrySource `json:"source"`
	Name       string      `json:"name"`
	CreatedAt  time.Time   `json:"createdAt"`
	ScannedAt  *time.Time  `json:"scannedAt,omitempty"`
}

// EntryConflictError is a second entry the same day: the name of who is
// already in, for the door to say so.
type EntryConflictError struct{ Name string }

func (e *EntryConflictError) Error() string { return "ticket: already entered today" }

// Unwrap makes it ErrConflict.
func (e *EntryConflictError) Unwrap() error { return ErrConflict }

// EntryDay is a day's Venue entries counted (GET
// /v1/events/{id}/entries/summary).
type EntryDay struct {
	Date       string         `json:"date"`
	EventDayID *uuid.UUID     `json:"eventDayId,omitempty"`
	Total      int            `json:"total"`
	BySource   map[string]int `json:"bySource"`
}

// EntrySummary is the Event's Venue entries counted by day.
type EntrySummary struct {
	Days []EntryDay `json:"days"`
}

// entryBy is how a door write enters the venue: the source a new Venue
// entry gets and the door staff who wrote it (nil for the person's own).
type entryBy struct {
	source EntrySource
	by     *uuid.UUID
}

// staffEntry is a write by the door staff p.
func staffEntry(source EntrySource, p authz.Principal) entryBy {
	how := entryBy{source: source}
	if id, err := uuid.Parse(p.ID); err == nil {
		how.by = &id
	}
	return how
}

// selfEntry is a write by the person themselves (the attendance QR,
// check-in/me, a guest's own).
var selfEntry = entryBy{source: EntryAttendance}

var istanbul = func() *time.Location {
	loc, err := time.LoadLocation("Europe/Istanbul")
	if err != nil {
		return time.FixedZone("TRT", 3*60*60)
	}
	return loc
}()

// IstanbulDate is t's Europe/Istanbul calendar day, YYYY-MM-DD.
func IstanbulDate(t time.Time) string {
	return t.In(istanbul).Format(time.DateOnly)
}

// Clock is the service's time (a NewService extra); time.Now without one.
type Clock func() time.Time

// shortEvent is how long an Event may last to be one venue day without an
// EventDay: one that runs past midnight (19:00 → 07:00) is still one night.
const shortEvent = 24 * time.Hour

// entryDay is the venue day at falls on (docs/venue-entry.md "The day"):
// the start day (Europe/Istanbul) and id of the EventDay whose window holds
// at (its start up to its end, or 24 hours without an end); else, on an
// Event that lasts 24 hours or less (one with no end does), the Event's
// start day; else at's calendar day. A night that runs past midnight is
// one day.
func entryDay(ev event.Event, days []event.Day, at time.Time) (string, *uuid.UUID) {
	for _, day := range days {
		if day.ArchivedAt != nil || day.StartDate == nil || at.Before(*day.StartDate) {
			continue
		}
		end := day.StartDate.Add(24 * time.Hour)
		if day.EndDate != nil && day.EndDate.After(*day.StartDate) {
			end = *day.EndDate
		}
		if at.Before(end) {
			id := day.ID
			return IstanbulDate(*day.StartDate), &id
		}
	}
	if ev.StartDate != nil {
		if end := ev.EndsAt(); end != nil && end.Sub(*ev.StartDate) <= shortEvent {
			return IstanbulDate(*ev.StartDate), nil
		}
	}
	return IstanbulDate(at), nil
}

// eventDay is entryDay over ev's days.
func (s *service) eventDay(ctx context.Context, ev event.Event, at time.Time) (string, *uuid.UUID, error) {
	days, err := s.events.ListDays(ctx, ev.ID)
	if err != nil {
		return "", nil, err
	}
	date, dayID := entryDay(ev, days, at)
	return date, dayID, nil
}

// sessionEntryDate is the venue day of a Check-in at sess on day: the
// EventDay's start day; an EventDay with no start falls back to entryDay
// at the Session's start (now without one), so a night's Sessions on both
// sides of midnight are one day.
func (s *service) sessionEntryDate(ctx context.Context, eventID uuid.UUID, day event.Day, sess event.Session) (string, error) {
	if day.StartDate != nil {
		return IstanbulDate(*day.StartDate), nil
	}
	at := s.scanTime(ctx)
	if sess.StartTime != nil {
		at = *sess.StartTime
	}
	ev, err := s.eventOf(ctx, eventID)
	if err != nil {
		return "", err
	}
	date, _, err := s.eventDay(ctx, ev, at)
	return date, err
}

// venueEntryFor is the Venue entry a door write at sess on day would add
// for t.
func (s *service) venueEntryFor(ctx context.Context, t Ticket, day event.Day, sess event.Session, how entryBy) (*VenueEntry, error) {
	date, err := s.sessionEntryDate(ctx, t.EventID, day, sess)
	if err != nil {
		return nil, err
	}
	dayID := day.ID
	scannedAt, late := s.scanned(ctx)
	return &VenueEntry{
		TicketID: t.ID, EventID: t.EventID, EntryDate: date,
		EventDayID: &dayID, Source: how.source, ByUserID: how.by, ScannedAt: scannedAt, NoMail: late,
	}, nil
}

// enterAtSession is a scan at a Session's door in the venue mode: the
// day's Venue entry alone (source scan), answered as the scan's check-in
// with kind entry. A second scan the same day is ErrConflict.
func (s *service) enterAtSession(ctx context.Context, t Ticket, sessionID uuid.UUID, how entryBy) (CheckIn, error) {
	sess, day, err := s.ticketSessionDay(ctx, t, sessionID)
	if err != nil {
		return CheckIn{}, err
	}
	if refused := inactiveErr(t); refused != nil {
		return CheckIn{}, refused
	}
	want, err := s.venueEntryFor(ctx, t, day, sess, how)
	if err != nil {
		return CheckIn{}, err
	}
	entry, err := s.tickets.AddVenueEntry(ctx, *want)
	if err != nil {
		return CheckIn{}, s.duplicate(ctx, err, t.ID, want.EntryDate, sessionID)
	}
	if entry.ScannedAt != nil {
		s.scans.recordOffline(nil)
	}
	s.scans.recordEntry(entry.Source)
	return CheckIn{
		ID: entry.ID, TicketID: t.ID, SessionID: sessionID, EventDayID: day.ID,
		CreatedAt: entry.CreatedAt, ScannedAt: entry.ScannedAt, Kind: KindEntry,
	}, nil
}

// venueMode reports whether ev's door writes the day's Venue entry alone.
func venueMode(ev event.Event) bool {
	return ev.EffectiveEntryMode == event.EntryVenue
}

// emptyBySource is a day's counts by source, every source at 0.
func emptyBySource() map[string]int {
	out := make(map[string]int, len(entrySources))
	for _, source := range entrySources {
		out[string(source)] = 0
	}
	return out
}
