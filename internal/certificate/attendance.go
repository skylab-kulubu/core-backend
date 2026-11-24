package certificate

import (
	"context"
	"errors"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// Attendance is an Event's attendance as the certificate rule reads it
// (docs/certificate-rule.md), read in a fixed number of queries however many
// Tickets the Event has.
type Attendance struct {
	// CountedTotal is N: the live counted Sessions.
	CountedTotal int
	// Required are the live required Sessions, in the program's order.
	Required []RequiredSession
	Tickets  []TicketAttendance
	// LiveSessions counts the live Sessions, counted or not; LastSessionEnd
	// is the latest end among them (a Session's end time, else its day's
	// end, else its Event's), nil when none has one. Attendance closes by
	// itself AutoFinalizeDelay after it, and may be closed by hand from it.
	LiveSessions   int
	LastSessionEnd *time.Time
	// Sessionless: the Event has no live Session (docs/certificate-rule.md,
	// "An Event with no Session"). The Event itself is then its one counted
	// unit (CountedTotal 1), a Ticket attended it when it has a Venue entry
	// (Attended 1), and LastSessionEnd is the Event's end once any Ticket
	// has entered. Its certificates keep no attendance snapshot.
	Sessionless bool
}

// venueCounted turns a with no live Session into the sessionless count:
// N = 1, and entered (the active Tickets with a Venue entry) attended it.
// ends is the Event's end; it closes the attendance only once someone
// entered, so an Event nobody scanned is never closed by the clock.
func (a *Attendance) venueCounted(entered map[uuid.UUID]bool, ends *time.Time) {
	if a.LiveSessions > 0 {
		return
	}
	a.Sessionless = true
	a.CountedTotal = 1
	someone := false
	for i := range a.Tickets {
		if entered[a.Tickets[i].TicketID] {
			a.Tickets[i].Attended = 1
			someone = true
		}
	}
	if someone && ends != nil {
		end := *ends
		a.LastSessionEnd = &end
	}
}

// addLiveSession counts a live Session that ends at ends (nil: unknown).
func (a *Attendance) addLiveSession(ends *time.Time) {
	a.LiveSessions++
	if ends != nil && (a.LastSessionEnd == nil || ends.After(*a.LastSessionEnd)) {
		end := *ends
		a.LastSessionEnd = &end
	}
}

// RequiredSession is a live required Session.
type RequiredSession struct {
	ID    uuid.UUID `json:"id"`
	Title string    `json:"title"`
}

// TicketAttendance is one Ticket's attendance and certificate state.
type TicketAttendance struct {
	TicketID   uuid.UUID
	TicketType string
	// Name is who the certificate would name: a Guest's name on the
	// Ticket, a member's account name (recipient's resolution). Empty when
	// there is none.
	Name string
	// Attended is the Ticket's check-ins on live counted Sessions.
	Attended int
	// ProgramMinutes sums the program length (end − start, in minutes) of
	// the live counted Sessions it checked in on: a Session without both
	// times adds 0, one that ends before it starts adds 0.
	ProgramMinutes int
	// MissingRequired are the live required Sessions it has no check-in on.
	MissingRequired []uuid.UUID
	// Active is its active certificate; Revoked its latest revoked one, only
	// when it has no active one.
	Active  *CertificateRef
	Revoked *CertificateRef
	// Job is its latest certificate job's state: JobQueued (queued or
	// running), JobFailed, or empty.
	Job string
}

// CertificateRef is a certificate as the attendance list shows it.
type CertificateRef struct {
	Serial    string     `json:"serial"`
	IssuedAt  time.Time  `json:"issuedAt"`
	RevokedAt *time.Time `json:"revokedAt,omitempty"`
}

const (
	JobQueued = "queued"
	JobFailed = "failed"
)

// Attendance list statuses, in their order of precedence.
const (
	StatusIssued   = "issued"
	StatusQueued   = "queued"
	StatusFailed   = "failed"
	StatusRevoked  = "revoked"
	StatusEligible = "eligible"
	StatusBelow    = "below"
)

// AttendanceStore reads an Event's attendance: the live counted and required
// Sessions once, and every Ticket's counted check-ins, missing required
// Sessions, certificates and latest job in one query.
type AttendanceStore interface {
	EventAttendance(ctx context.Context, eventID uuid.UUID) (Attendance, error)
}

// AttendanceList is the certificate workspace's list of who attended how
// much (GET /v1/events/{id}/certificates/attendance). It carries no e-mail.
type AttendanceList struct {
	CountedTotal int    `json:"countedTotal"`
	EffectiveMin int    `json:"effectiveMin"`
	Rule         string `json:"rule"`
	// Sessionless: the Event has no live Session; attended 1 is a Venue
	// entry (docs/certificate-rule.md).
	Sessionless bool              `json:"sessionless"`
	Required    []RequiredSession `json:"required"`
	Rows        []AttendanceRow   `json:"rows"`
}

// AttendanceRow is one Ticket on the attendance list.
type AttendanceRow struct {
	TicketID        uuid.UUID         `json:"ticketId"`
	Name            string            `json:"name"`
	TicketType      string            `json:"ticketType"`
	Attended        int               `json:"attended"`
	MissingRequired []RequiredSession `json:"missingRequired"`
	Status          string            `json:"status"`
	Certificate     *CertificateRef   `json:"certificate"`
}

// MaxJustification is the longest justification a manual issue keeps, in
// characters (certificate_batches.justification's check).
const MaxJustification = 500

var (
	// ErrJustificationRequired: a manual issue to a Ticket the rule does not
	// make eligible needs a justification.
	ErrJustificationRequired = errors.New("certificate: justification required")
	// ErrJustificationTooLong: longer than MaxJustification characters.
	ErrJustificationTooLong = errors.New("certificate: justification too long")
)

// ticketEligible decides a Ticket by the Event's rule over its attendance.
func ticketEligible(ev event.Event, a Attendance, t TicketAttendance) bool {
	return Eligible(RuleOf(ev), t.Attended, a.CountedTotal, len(t.MissingRequired))
}

// attendanceStatus is a Ticket's status on the list: issued > queued >
// failed > revoked > eligible > below.
func attendanceStatus(eligible bool, t TicketAttendance) string {
	switch {
	case t.Active != nil:
		return StatusIssued
	case t.Job == JobQueued:
		return StatusQueued
	case t.Job == JobFailed:
		return StatusFailed
	case t.Revoked != nil:
		return StatusRevoked
	case eligible:
		return StatusEligible
	default:
		return StatusBelow
	}
}

// ticketOf is the Ticket's attendance, and whether the Event has it.
func (a Attendance) ticketOf(ticketID uuid.UUID) (TicketAttendance, bool) {
	for _, t := range a.Tickets {
		if t.TicketID == ticketID {
			return t, true
		}
	}
	return TicketAttendance{}, false
}

// normalizeJustification trims it and refuses one over MaxJustification
// characters; empty is none.
func normalizeJustification(raw string) (*string, error) {
	text := strings.TrimSpace(raw)
	if text == "" {
		return nil, nil
	}
	if utf8.RuneCountInString(text) > MaxJustification {
		return nil, ErrJustificationTooLong
	}
	return &text, nil
}

// sortAttendanceRows orders the list by name, then by Ticket, so that the
// memory and Postgres stores answer the same order.
func sortAttendanceRows(rows []AttendanceRow) {
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := strings.ToLower(rows[i].Name), strings.ToLower(rows[j].Name)
		if a != b {
			return a < b
		}
		return rows[i].TicketID.String() < rows[j].TicketID.String()
	})
}

// latestJobReader is the memory store's latest job per Ticket.
type latestJobReader interface {
	latestJobs(eventID uuid.UUID) map[uuid.UUID]string
}

// memoryAttendance reads attendance from the stores' interfaces: the memory
// counterpart of PostgresStore.EventAttendance, for stores that have no
// query of their own. It reads the program once per Event, not per Ticket.
type memoryAttendance struct {
	events  event.Store
	tickets ticket.Store
	certs   Store
	jobs    JobStore
	users   user.Store
}

func (m memoryAttendance) EventAttendance(ctx context.Context, eventID uuid.UUID) (Attendance, error) {
	days, err := m.events.ListDays(ctx, eventID)
	if err != nil {
		return Attendance{}, err
	}
	ev, err := m.events.Get(ctx, eventID)
	if err != nil {
		return Attendance{}, err
	}
	out := Attendance{Required: []RequiredSession{}, Tickets: []TicketAttendance{}}
	counted := map[uuid.UUID]bool{}
	minutes := map[uuid.UUID]int{}
	for _, day := range days {
		sessions, err := m.events.ListSessions(ctx, day.ID)
		if err != nil {
			return Attendance{}, err
		}
		for _, sess := range sessions {
			if sess.Cancelled {
				continue
			}
			out.addLiveSession(sessionEnds(ev, day, sess))
			if sess.CertificateCounted {
				counted[sess.ID] = true
				minutes[sess.ID] = programMinutes(sess)
				out.CountedTotal++
			}
			if sess.CertificateRequired {
				out.Required = append(out.Required, RequiredSession{ID: sess.ID, Title: sess.Title})
			}
		}
	}
	tickets, err := m.tickets.ListByEvent(ctx, eventID)
	if err != nil {
		return Attendance{}, err
	}
	certs, err := m.certs.ListByEvent(ctx, eventID)
	if err != nil {
		return Attendance{}, err
	}
	var jobs map[uuid.UUID]string
	if reader, ok := m.jobs.(latestJobReader); ok {
		jobs = reader.latestJobs(eventID)
	}
	for _, t := range tickets {
		// Active Tickets only, as the database's (docs/ticket-cancellation.md).
		if !t.Active() {
			continue
		}
		row := TicketAttendance{TicketID: t.ID, TicketType: t.TicketType, MissingRequired: []uuid.UUID{}, Job: jobs[t.ID]}
		row.Name = m.name(ctx, t)
		seen := map[uuid.UUID]bool{}
		for _, checkIn := range t.CheckIns {
			if counted[checkIn.SessionID] && !seen[checkIn.SessionID] {
				row.Attended++
				row.ProgramMinutes += minutes[checkIn.SessionID]
			}
			seen[checkIn.SessionID] = true
		}
		for _, required := range out.Required {
			if !seen[required.ID] {
				row.MissingRequired = append(row.MissingRequired, required.ID)
			}
		}
		for _, c := range certs {
			if c.TicketID != t.ID {
				continue
			}
			ref := &CertificateRef{Serial: c.Serial, IssuedAt: c.IssuedAt, RevokedAt: c.RevokedAt}
			if c.RevokedAt == nil {
				row.Active = ref
			} else if row.Revoked == nil || c.RevokedAt.After(*row.Revoked.RevokedAt) {
				row.Revoked = ref
			}
		}
		if row.Active != nil {
			row.Revoked = nil
		}
		out.Tickets = append(out.Tickets, row)
	}
	if out.LiveSessions == 0 {
		entered, err := m.entered(ctx, eventID)
		if err != nil {
			return Attendance{}, err
		}
		out.venueCounted(entered, ev.EndsAt())
	}
	return out, nil
}

// entered are the Event's Tickets with a Venue entry, read day by day from
// the door's own lists.
func (m memoryAttendance) entered(ctx context.Context, eventID uuid.UUID) (map[uuid.UUID]bool, error) {
	days, err := m.tickets.VenueEntryDays(ctx, eventID)
	if err != nil {
		return nil, err
	}
	out := map[uuid.UUID]bool{}
	for _, day := range days {
		entries, err := m.tickets.VenueEntriesOn(ctx, eventID, day.Date)
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			out[e.TicketID] = true
		}
	}
	return out, nil
}

func (m memoryAttendance) name(ctx context.Context, t ticket.Ticket) string {
	if t.TicketType == ticket.Guest {
		return strings.TrimSpace(t.GuestFirstName + " " + t.GuestLastName)
	}
	if t.OwnerID == nil || m.users == nil {
		return ""
	}
	u, err := m.users.Get(ctx, *t.OwnerID)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(u.FirstName + " " + u.LastName)
}

// sessionEnds is a Session's end as sessionEndsSQL reads it: its end time,
// else its day's end, else its Event's (event.Event.EndsAt).
func sessionEnds(ev event.Event, day event.Day, sess event.Session) *time.Time {
	switch {
	case sess.EndTime != nil:
		return sess.EndTime
	case day.EndDate != nil:
		return day.EndDate
	default:
		return ev.EndsAt()
	}
}

// programMinutes is a Session's program length in whole minutes as
// programMinutesSQL reads it: 0 without both times or when it ends before
// it starts.
func programMinutes(sess event.Session) int {
	if sess.StartTime == nil || sess.EndTime == nil || !sess.EndTime.After(*sess.StartTime) {
		return 0
	}
	return int(sess.EndTime.Sub(*sess.StartTime) / time.Minute)
}

// snapshotOf is a Ticket's attendance snapshot at its certificate's issue;
// a Ticket the attendance does not list attended none of the N Sessions.
func (a Attendance) snapshotOf(ticketID uuid.UUID) *AttendanceSnapshot {
	if a.Sessionless {
		// "1 oturumdan 1'i" would name a Session the Event never had.
		return nil
	}
	row, _ := a.ticketOf(ticketID)
	return &AttendanceSnapshot{Attended: row.Attended, CountedTotal: a.CountedTotal, ProgramMinutes: row.ProgramMinutes}
}
