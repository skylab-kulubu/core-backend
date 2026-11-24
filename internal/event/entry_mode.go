package event

// EntryMode is what a door scan writes at an Event (docs/venue-entry.md):
// a Check-in to the Session the door picked, or a Venue entry for the day.
type EntryMode string

const (
	// EntryPerSession: the door's scan checks the person in to the Session
	// it picked (and writes the day's Venue entry if there is none yet).
	EntryPerSession EntryMode = "per_session"
	// EntryVenue: the door's scan writes the day's Venue entry alone; the
	// Sessions take their attendance by the attendance QR.
	EntryVenue EntryMode = "venue"
)

// Valid reports whether m is one of the two modes.
func (m EntryMode) Valid() bool {
	return m == EntryPerSession || m == EntryVenue
}

// validEntryMode reports whether a written mode is nil (automatic) or one of
// the two.
func validEntryMode(m *EntryMode) bool {
	return m == nil || m.Valid()
}

// EffectiveEntryMode is the mode the door works in: venue on an Event with
// no live Session (a door that picks no Session can only write the day's
// entry), else the organizer's choice, per_session when they made none.
func EffectiveEntryMode(chosen *EntryMode, liveSessions bool) EntryMode {
	if !liveSessions {
		return EntryVenue
	}
	if chosen != nil && chosen.Valid() {
		return *chosen
	}
	return EntryPerSession
}

// liveSessionsSQL is whether the Event e has a live Session: on a current
// day, current and not cancelled (the Sessions the certificate rule
// counts from).
const liveSessionsSQL = `EXISTS (SELECT 1 FROM event_days ld
	JOIN sessions ls ON ls.event_day_id = ld.id AND ls.archived_at IS NULL AND NOT ls.cancelled
	WHERE ld.event_id = e.id AND ld.archived_at IS NULL)`
