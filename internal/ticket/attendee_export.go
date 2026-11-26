package ticket

import (
	"context"
	"errors"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/teamaudit"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// An attendee export (docs/attendee-export.md): the panel downloads a
// report of an Event's attendees as a file. The ticket service decides who
// may (a reader of the Event's Tickets: never its door staff alone), reads
// the Tickets, their people, the program and the Venue entries, and keeps
// the Team audit row (ticket.export); package attendeeexport makes the file.

// The reports an export makes.
const (
	ExportAttendees    = "attendees"
	ExportSession      = "session"
	ExportMatrix       = "matrix"
	ExportEntries      = "entries"
	ExportCertificates = "certificates"
)

// ExportReports are every report, in the panel's order.
var ExportReports = []string{ExportAttendees, ExportSession, ExportMatrix, ExportEntries, ExportCertificates}

// MaxExportSelection is the most Tickets a selected export names.
const MaxExportSelection = 20000

// ExportRequest is what the panel asks for.
type ExportRequest struct {
	Report string
	Format string
	// SessionID is the Session of a session report.
	SessionID uuid.UUID
	// Selected: the rows the panel shows (TicketIDs), not the whole Event.
	Selected  bool
	TicketIDs []uuid.UUID
}

// ExportPerson is who a Ticket names, as the export writes them: a
// member's account name, e-mail, university and department, a guest's
// own (with the phone, university, department and grade a Walk-in or a
// form gave, docs/walk-in.md); an erased account writes
// user.DeletedDisplayName and nothing else.
type ExportPerson struct {
	FirstName  string
	LastName   string
	Email      string
	Phone      string
	University string
	Department string
	// Grade is one of Grades, or empty.
	Grade string
}

// Name is the person's full name.
func (p ExportPerson) Name() string {
	return strings.TrimSpace(p.FirstName + " " + p.LastName)
}

// ExportData is what an export reads: every Ticket of the Event whatever
// its status (with its Check-ins), its person, the live program, and the
// Venue entries of every day.
type ExportData struct {
	Event   event.Event
	Request ExportRequest
	Tickets []Ticket
	People  map[uuid.UUID]ExportPerson
	// Days and Sessions are the program: current days, and their Sessions
	// that are not archived (cancelled ones included, marked).
	Days     []event.Day
	Sessions []event.Session
	Entries  []DoorEntry
	// Certificates: the caller may read the Event's certificates; without
	// it no certificate column is written and the certificate report is
	// refused.
	Certificates bool
	audit        *auditRecord
}

// ErrExportInvalid is a request no report answers: an unknown report or
// format, a session report without its Session, too many Tickets.
var ErrExportInvalid = errors.New("ticket: invalid export")

func validExport(req ExportRequest) bool {
	if !slices.Contains(ExportReports, req.Report) || (req.Format != "xlsx" && req.Format != "csv") {
		return false
	}
	if (req.Report == ExportSession) != (req.SessionID != uuid.Nil) {
		return false
	}
	return len(req.TicketIDs) <= MaxExportSelection && (req.Selected || len(req.TicketIDs) == 0)
}

func (s *service) BeginExport(ctx context.Context, p authz.Principal, eventID uuid.UUID, req ExportRequest) (*ExportData, error) {
	if !validExport(req) {
		return nil, ErrExportInvalid
	}
	ev, err := s.events.Get(ctx, eventID)
	if errors.Is(err, event.ErrNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if err := s.seen(&p, ev); err != nil {
		return nil, err
	}
	// A reader of the Event's Tickets; the door staff check people in and
	// read no list (authz: "door staff cannot list event tickets").
	if p.ID == "" || p.Product != "" || !s.authz.Allow(p, eventResource(ev, authz.TypeTicket), authz.Read) {
		return nil, ErrForbidden
	}
	certificates := s.authz.Allow(p, eventResource(ev, authz.TypeCertificate), authz.Read)
	if req.Report == ExportCertificates && !certificates {
		return nil, ErrForbidden
	}
	out := &ExportData{Event: ev, Request: req, Certificates: certificates, People: map[uuid.UUID]ExportPerson{}}
	if out.Days, err = s.events.ListDays(ctx, eventID); err != nil {
		return nil, err
	}
	found := req.SessionID == uuid.Nil
	for _, day := range out.Days {
		sessions, err := s.events.ListSessions(ctx, day.ID)
		if err != nil {
			return nil, err
		}
		for _, sess := range sessions {
			found = found || sess.ID == req.SessionID
		}
		out.Sessions = append(out.Sessions, sessions...)
	}
	if !found {
		return nil, ErrNotFound
	}
	store, err := s.statusStore()
	if err != nil {
		return nil, err
	}
	if out.Tickets, err = store.ListByEventStatus(ctx, eventID, nil); err != nil {
		return nil, err
	}
	if req.Selected {
		wanted := make(map[uuid.UUID]bool, len(req.TicketIDs))
		for _, id := range req.TicketIDs {
			wanted[id] = true
		}
		out.Tickets = slices.DeleteFunc(out.Tickets, func(t Ticket) bool { return !wanted[t.ID] })
	}
	if err := s.exportPeople(ctx, out); err != nil {
		return nil, err
	}
	if req.Report == ExportEntries || req.Report == ExportAttendees {
		days, err := s.tickets.VenueEntryDays(ctx, eventID)
		if err != nil {
			return nil, err
		}
		for _, day := range days {
			entries, err := s.tickets.VenueEntriesOn(ctx, eventID, day.Date)
			if err != nil {
				return nil, err
			}
			out.Entries = append(out.Entries, entries...)
		}
	}
	// The row is opened once the request is known good; a reader who is
	// neither Privileged nor a Leader is recorded as an event editor
	// through the Event's team, as an import is.
	privileged := s.authz.Allow(p, authz.Resource{Type: authz.TypeTicket}, authz.Read)
	if out.audit, err = s.beginTicketAuditBasis(ctx, p, ev, teamaudit.ActionTicketExport, uuid.Nil, privileged, true); err != nil {
		return nil, err
	}
	return out, nil
}

// exportPeople names each Ticket's person: a guest's own fields, a
// member's account (one read for all of them).
func (s *service) exportPeople(ctx context.Context, out *ExportData) error {
	owners := make([]uuid.UUID, 0, len(out.Tickets))
	for _, t := range out.Tickets {
		if t.OwnerID != nil {
			owners = append(owners, *t.OwnerID)
		}
	}
	var accounts map[uuid.UUID]user.Account
	if len(owners) > 0 && s.users != nil {
		var err error
		if accounts, err = s.users.Accounts(ctx, owners); err != nil {
			return err
		}
	}
	for _, t := range out.Tickets {
		if t.OwnerID == nil {
			out.People[t.ID] = ExportPerson{
				FirstName: t.GuestFirstName, LastName: t.GuestLastName, Email: t.GuestEmail, Phone: t.GuestPhoneNumber,
				University: t.GuestUniversity, Department: t.GuestDepartment, Grade: t.GuestGrade,
			}
			continue
		}
		a, ok := accounts[*t.OwnerID]
		switch {
		case accounts == nil:
			// No account store (a test): the member stays unnamed.
			out.People[t.ID] = ExportPerson{}
		case !ok || a.Blocked || !a.Stored:
			out.People[t.ID] = ExportPerson{FirstName: user.DeletedDisplayName}
		default:
			out.People[t.ID] = ExportPerson{
				FirstName: a.User.FirstName, LastName: a.User.LastName, Email: a.User.Email,
				University: a.User.University, Department: a.User.Department,
			}
		}
	}
	return nil
}

// FinishExport closes the export's audit row: ok with the file's rows, or
// failed when the file could not be made.
func (s *service) FinishExport(ctx context.Context, data *ExportData, rows int, err error) {
	if data == nil || data.audit == nil {
		return
	}
	outcome, code := teamaudit.OutcomeOK, ""
	if err != nil {
		outcome, code = teamaudit.OutcomeFailed, "failed"
	}
	after := data.audit.after
	after.Export = &teamaudit.ExportInfo{
		Report: data.Request.Report, Format: data.Request.Format, Selected: data.Request.Selected, Rows: rows,
	}
	if data.Request.SessionID != uuid.Nil {
		after.Export.SessionID = data.Request.SessionID.String()
	}
	if ferr := s.audit.Finish(ctx, data.audit.id, outcome, code, &after); ferr != nil && s.logger != nil {
		s.logger.Printf("tickets: audit record %s stays pending", data.audit.id)
	}
}
