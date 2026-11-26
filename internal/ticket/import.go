package ticket

import (
	"context"
	"errors"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/teamaudit"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// The attendee import (docs/attendee-import.md): an operator of an Event
// turns a list of people who registered elsewhere (Microsoft Forms, a
// spreadsheet) into Tickets. A preview says what the list would do; the
// import writes it. Both read the list anew, keep nothing of it, and write
// one audit row of counts. The same list imported twice writes nothing the
// second time: a person who already holds a Ticket on the Event is left as
// they are.

// AttendeeImport is the import's two steps.
type AttendeeImport interface {
	PreviewImport(ctx context.Context, p authz.Principal, eventID uuid.UUID, in ImportInput) (ImportReport, error)
	ImportAttendees(ctx context.Context, p authz.Principal, eventID uuid.UUID, in ImportInput) (ImportReport, error)
}

// ImportInput is what the operator sends.
type ImportInput struct {
	// Text is the list: a CSV file's text or rows pasted from a spreadsheet.
	Text string
	// SendMail queues each new Ticket's mail ("Biletin hazır", or
	// "Bekleme listesindesin" while the status mails are on), as a Ticket
	// written any other way. Off, nobody is mailed now.
	SendMail bool
	// PastCapacity writes every new Ticket active, past the capacity if it
	// must, as "Katılımcı ekle" does. Off, the capacity and the waiting
	// list decide (docs/event-capacity.md).
	PastCapacity bool
}

// ImportOutcome is what a row does (preview) or did (import).
type ImportOutcome string

const (
	// ImportActive: a Ticket written active.
	ImportActive ImportOutcome = "active"
	// ImportWaitlisted: a Ticket written on the waiting list.
	ImportWaitlisted ImportOutcome = "waitlisted"
	// ImportFull: no Ticket, the Event is full and its waiting list off.
	ImportFull ImportOutcome = "full"
	// ImportExisting: the person already holds a Ticket on the Event
	// (active, waitlisted or cancelled: TicketStatus says which); left as
	// it is.
	ImportExisting ImportOutcome = "existing"
	// ImportDuplicate: the e-mail is on an earlier row of the list.
	ImportDuplicate ImportOutcome = "duplicate"
	// ImportInvalid: a row that cannot hold a Ticket (Problem says why).
	ImportInvalid ImportOutcome = "invalid"
)

// The reasons a row is invalid.
const (
	ImportProblemFirstName = "missing_first_name"
	ImportProblemLastName  = "missing_last_name"
	ImportProblemEmail     = "invalid_email"
	ImportProblemNameLong  = "name_too_long"
	// ImportProblemCharacters is a row with a control character (NUL
	// included) or a byte that was not UTF-8 (a Windows-1254 CSV) in its
	// name or address.
	ImportProblemCharacters = "invalid_characters"
	// ImportProblemRejected is a row the database refused to store (a
	// value it does not take): that row alone is left out.
	ImportProblemRejected = "rejected"
	// ImportProblemAccount is an account that was there at the preview
	// and is gone at the import.
	ImportProblemAccount = "account_unavailable"
)

// ImportSession is a Session a row chose.
type ImportSession struct {
	ID    uuid.UUID `json:"id"`
	Title string    `json:"title"`
}

// ImportRow is one row of the list and its outcome. The answer carries the
// row back to the operator who sent it; core keeps none of it.
type ImportRow struct {
	// Line is the row's line in the list, the header's being 1.
	Line      int    `json:"line"`
	FirstName string `json:"firstName"`
	LastName  string `json:"lastName"`
	Email     string `json:"email"`
	// Sessions are the Event's Sessions the row named; UnknownSessions
	// those it named that the Event does not have. Neither changes the
	// Ticket, which is the Event's (a Ticket is not tied to a Session).
	Sessions        []ImportSession `json:"sessions,omitempty"`
	UnknownSessions []string        `json:"unknownSessions,omitempty"`
	Outcome         ImportOutcome   `json:"outcome"`
	Problem         string          `json:"problem,omitempty"`
	// Account: an account holds the e-mail; its Ticket is the account's
	// (a member's Ticket), not a guest's.
	Account bool `json:"account"`
	// TicketStatus is the Ticket already there, on an existing row.
	TicketStatus Status `json:"ticketStatus,omitempty"`
	// OverCapacity: the Ticket is (or would be) active past the capacity.
	OverCapacity bool `json:"overCapacity,omitempty"`

	ownerID *uuid.UUID
}

// ImportSummary counts the rows by outcome.
type ImportSummary struct {
	Rows            int `json:"rows"`
	Active          int `json:"active"`
	Waitlisted      int `json:"waitlisted"`
	Full            int `json:"full"`
	Existing        int `json:"existing"`
	Duplicates      int `json:"duplicates"`
	Invalid         int `json:"invalid"`
	OverCapacity    int `json:"overCapacity"`
	Members         int `json:"members"`
	Guests          int `json:"guests"`
	UnknownSessions int `json:"unknownSessions"`
}

// ImportCapacity is the Event's capacity when the list was read.
type ImportCapacity struct {
	// Capacity is the bound, 0 for none; Active the active Tickets; Free
	// the places left, -1 for no bound.
	Capacity        int  `json:"capacity"`
	Active          int  `json:"active"`
	Free            int  `json:"free"`
	WaitlistEnabled bool `json:"waitlistEnabled"`
}

// The new Tickets' mail, as the answer tells it.
const (
	// ImportMailQueued: each new Ticket's mail is (or would be) queued.
	ImportMailQueued = "queued"
	// ImportMailOff: the operator turned the mail off.
	ImportMailOff = "off"
	// ImportMailNotificationsOff: core sends no Ticket mail now
	// (TICKET_CREATED_NOTIFICATION).
	ImportMailNotificationsOff = "notifications_off"
	// ImportMailOnPublish: the Event is a draft; its Tickets are told of
	// only if its publication asks (docs/ticket-created-notification.md).
	ImportMailOnPublish = "on_publish"
)

// ImportReport is the answer of a preview and of an import.
type ImportReport struct {
	// Imported: the Tickets were written (an import); false for a preview.
	Imported bool           `json:"imported"`
	Columns  ImportColumns  `json:"columns"`
	Summary  ImportSummary  `json:"summary"`
	Capacity ImportCapacity `json:"capacity"`
	Mail     string         `json:"mail"`
	Rows     []ImportRow    `json:"rows"`
}

// ImportMatch is what core holds for one e-mail on an Event.
type ImportMatch struct {
	// Accounts are the active accounts whose e-mail it is.
	Accounts []uuid.UUID
	// Ticket is the status of the Event's Ticket the e-mail holds, as a
	// guest or through its account; "" for none. An active one wins over
	// a waitlisted one, which wins over a cancelled one.
	Ticket Status
}

// ImportMatcher reads the matches of a whole list in one go (the
// database's). Without it the service asks one e-mail at a time.
type ImportMatcher interface {
	ImportMatches(ctx context.Context, eventID uuid.UUID, emails []string) (map[string]ImportMatch, error)
}

// PreviewImport reads the list and says what importing it would do, writing
// nothing.
//
// The preview is recorded too (ticket.import.preview): who read which
// Event's list and its counts, never a row. A row that cannot be opened
// stops it, as every audited action.
func (s *service) PreviewImport(ctx context.Context, p authz.Principal, eventID uuid.UUID, in ImportInput) (ImportReport, error) {
	ev, report, err := s.planImport(ctx, p, eventID, in)
	if err != nil {
		return ImportReport{}, err
	}
	record, err := s.beginTicketAuditBasis(ctx, p, ev, teamaudit.ActionTicketImportPreview, uuid.Nil, s.privilegedOperator(p), true)
	if err != nil {
		return ImportReport{}, err
	}
	s.finishImportAudit(ctx, record, nil, report.Summary, in.SendMail)
	return report, nil
}

// ImportAttendees writes the list's Tickets: each new row under the Event's
// capacity (or past it with PastCapacity), its mail queued with SendMail.
// A row whose person got a Ticket since the preview is existing; one the
// Event has no place for now is full. The audit row is opened before the
// first write; one that cannot be opened stops the import.
func (s *service) ImportAttendees(ctx context.Context, p authz.Principal, eventID uuid.UUID, in ImportInput) (ImportReport, error) {
	ev, report, err := s.planImport(ctx, p, eventID, in)
	if err != nil {
		return ImportReport{}, err
	}
	store, err := s.capacityStore()
	if err != nil {
		return ImportReport{}, err
	}
	record, err := s.beginTicketAuditBasis(ctx, p, ev, teamaudit.ActionTicketImport, uuid.Nil, s.privilegedOperator(p), true)
	if err != nil {
		return ImportReport{}, err
	}
	admission := Admission{Waitlist: !in.PastCapacity, Over: in.PastCapacity, Quiet: !in.SendMail}
	var failed error
	done := len(report.Rows)
	for i := range report.Rows {
		row := &report.Rows[i]
		switch row.Outcome {
		case ImportActive, ImportWaitlisted, ImportFull:
		default:
			continue
		}
		t := Ticket{EventID: eventID}
		if row.ownerID != nil {
			t.TicketType, t.OwnerID = Registered, row.ownerID
		} else {
			t.TicketType = Guest
			t.GuestFirstName, t.GuestLastName, t.GuestEmail = row.FirstName, row.LastName, row.Email
		}
		write := func(t Ticket) (Ticket, error) { return store.CreateAdmitted(ctx, t, admission) }
		var created Ticket
		if t.TicketType == Guest {
			// A guest row whose address an account holds verified is
			// linked to it (docs/guest-account-links.md).
			created, err = s.createGuest(ctx, t, write)
		} else {
			created, err = write(t)
		}
		row.OverCapacity = false
		switch {
		case err == nil:
			row.Outcome = ImportActive
			if created.StatusOf() == StatusWaitlisted {
				row.Outcome = ImportWaitlisted
			}
			row.OverCapacity = created.OverCapacity
		case errors.Is(err, ErrConflict):
			// Written since the list was read (a second import at the same
			// moment, the person's own registration).
			row.Outcome, row.TicketStatus = ImportExisting, ""
		case errors.Is(err, ErrEventFull):
			row.Outcome = ImportFull
		case errors.Is(err, ErrNotFound) && row.ownerID != nil:
			// The account went away since the list was read.
			row.Outcome, row.Problem = ImportInvalid, ImportProblemAccount
		case rowRefused(err):
			// A value the database does not take: this row alone.
			row.Outcome, row.Problem = ImportInvalid, ImportProblemRejected
		default:
			failed, done = err, i
		}
		if failed != nil {
			break
		}
	}
	report.Imported = failed == nil
	report.Summary = summarize(report.Rows)
	// On a failure the audit row counts what was done: the rows before the
	// one that failed, none planned after it.
	counts := summarize(report.Rows[:done])
	counts.Rows = len(report.Rows)
	s.finishImportAudit(ctx, record, failed, counts, in.SendMail)
	if failed != nil {
		return ImportReport{}, failed
	}
	return report, nil
}

// planImport checks the caller, reads the list and says what each row would
// do now.
func (s *service) planImport(ctx context.Context, p authz.Principal, eventID uuid.UUID, in ImportInput) (event.Event, ImportReport, error) {
	ev, err := s.events.Get(ctx, eventID)
	if err != nil {
		if errors.Is(err, event.ErrNotFound) {
			return event.Event{}, ImportReport{}, ErrNotFound
		}
		return event.Event{}, ImportReport{}, err
	}
	// As "Katılımcı ekle": who may not see a draft gets 404, anybody who
	// does not run the Event 403, before the list is read.
	if err := s.seen(&p, ev); err != nil {
		return event.Event{}, ImportReport{}, err
	}
	if !s.operatorOf(p, ev) {
		return event.Event{}, ImportReport{}, ErrForbidden
	}
	if ev.Status == event.StatusCancelled {
		return event.Event{}, ImportReport{}, ErrEventCancelled
	}
	lines, cols, err := parseImport(in.Text)
	if err != nil {
		return event.Event{}, ImportReport{}, err
	}
	store, err := s.capacityStore()
	if err != nil {
		return event.Event{}, ImportReport{}, err
	}
	capacity, err := store.Capacity(ctx, eventID)
	if err != nil {
		return event.Event{}, ImportReport{}, err
	}
	sessions, err := s.importSessions(ctx, eventID)
	if err != nil {
		return event.Event{}, ImportReport{}, err
	}

	rows := make([]ImportRow, len(lines))
	seen := map[string]bool{}
	var emails []string
	for i, l := range lines {
		row := ImportRow{Line: l.line, FirstName: l.firstName, LastName: l.lastName, Email: l.email}
		for _, name := range l.sessions {
			if badCharacters(name) {
				continue
			}
			if sess, ok := sessions.find(name); ok {
				row.Sessions = append(row.Sessions, sess)
			} else {
				row.UnknownSessions = append(row.UnknownSessions, name)
			}
		}
		switch {
		case badCharacters(l.firstName) || badCharacters(l.lastName) || badCharacters(l.email):
			row.Outcome, row.Problem = ImportInvalid, ImportProblemCharacters
		case !validImportEmail(l.email):
			row.Outcome, row.Problem = ImportInvalid, ImportProblemEmail
		case l.firstName == "":
			row.Outcome, row.Problem = ImportInvalid, ImportProblemFirstName
		case l.lastName == "":
			row.Outcome, row.Problem = ImportInvalid, ImportProblemLastName
		case len([]rune(l.firstName)) > MaxImportName || len([]rune(l.lastName)) > MaxImportName:
			row.Outcome, row.Problem = ImportInvalid, ImportProblemNameLong
		case seen[l.email]:
			row.Outcome = ImportDuplicate
		default:
			seen[l.email] = true
			emails = append(emails, l.email)
		}
		rows[i] = row
	}
	matches, err := s.importMatches(ctx, eventID, emails)
	if err != nil {
		return event.Event{}, ImportReport{}, err
	}

	report := ImportReport{
		Columns: cols,
		Capacity: ImportCapacity{
			Capacity: capacity.Capacity, Active: capacity.Active, Free: capacity.Free(),
			WaitlistEnabled: capacity.Waitlist,
		},
		Mail: s.importMail(ev, in.SendMail),
	}
	for i := range rows {
		row := &rows[i]
		if row.Outcome != "" {
			continue
		}
		m := matches[row.Email]
		if len(m.Accounts) == 1 {
			id := m.Accounts[0]
			row.Account, row.ownerID = true, &id
		}
		if m.Ticket != "" {
			row.Outcome, row.TicketStatus = ImportExisting, m.Ticket
			continue
		}
		// The capacity as the rows before took it.
		status, over, err := admit(capacity, Admission{Waitlist: !in.PastCapacity, Over: in.PastCapacity})
		switch {
		case err != nil:
			row.Outcome = ImportFull
		case status == StatusWaitlisted:
			row.Outcome = ImportWaitlisted
		default:
			row.Outcome, row.OverCapacity = ImportActive, over
			capacity.Active++
		}
	}
	report.Rows = rows
	report.Summary = summarize(rows)
	return ev, report, nil
}

// importMail says what happens to the new Tickets' mail.
func (s *service) importMail(ev event.Event, requested bool) string {
	store, _ := s.tickets.(ResendStore)
	switch {
	case !requested:
		return ImportMailOff
	case store == nil || !store.NotificationsOn():
		return ImportMailNotificationsOff
	case ev.Status == event.StatusDraft:
		return ImportMailOnPublish
	}
	return ImportMailQueued
}

func summarize(rows []ImportRow) ImportSummary {
	sum := ImportSummary{Rows: len(rows)}
	for _, row := range rows {
		switch row.Outcome {
		case ImportActive:
			sum.Active++
		case ImportWaitlisted:
			sum.Waitlisted++
		case ImportFull:
			sum.Full++
		case ImportExisting:
			sum.Existing++
		case ImportDuplicate:
			sum.Duplicates++
		case ImportInvalid:
			sum.Invalid++
		}
		if row.OverCapacity {
			sum.OverCapacity++
		}
		if row.Outcome == ImportActive || row.Outcome == ImportWaitlisted {
			if row.Account {
				sum.Members++
			} else {
				sum.Guests++
			}
		}
		if len(row.UnknownSessions) > 0 {
			sum.UnknownSessions++
		}
	}
	return sum
}

// importMatches reads what core holds for each e-mail on the Event: in one
// query on the database, one e-mail at a time otherwise.
func (s *service) importMatches(ctx context.Context, eventID uuid.UUID, emails []string) (map[string]ImportMatch, error) {
	if len(emails) == 0 {
		return map[string]ImportMatch{}, nil
	}
	if m, ok := s.tickets.(ImportMatcher); ok {
		return m.ImportMatches(ctx, eventID, emails)
	}
	out := make(map[string]ImportMatch, len(emails))
	for _, email := range emails {
		var m ImportMatch
		if s.users != nil {
			found, err := s.users.FindByEmail(ctx, email)
			if err != nil {
				return nil, err
			}
			for _, u := range found {
				if u.AccountState == "" || u.AccountState == user.AccountActive {
					m.Accounts = append(m.Accounts, u.ID)
				}
			}
		}
		if t, err := s.tickets.GetByGuestEvent(ctx, email, eventID); err == nil {
			m.Ticket = strongerStatus(m.Ticket, t.StatusOf())
		} else if !errors.Is(err, ErrNotFound) {
			return nil, err
		}
		for _, id := range m.Accounts {
			t, err := s.tickets.GetByOwnerEvent(ctx, id, eventID)
			if err == nil {
				m.Ticket = strongerStatus(m.Ticket, t.StatusOf())
			} else if !errors.Is(err, ErrNotFound) {
				return nil, err
			}
		}
		out[email] = m
	}
	return out, nil
}

// strongerStatus is the status a match says of two Tickets: active, then
// waitlisted, then cancelled.
func strongerStatus(a, b Status) Status {
	rank := func(s Status) int {
		switch s {
		case StatusActive:
			return 3
		case StatusWaitlisted:
			return 2
		case StatusCancelled:
			return 1
		}
		return 0
	}
	if rank(b) > rank(a) {
		return b
	}
	return a
}

// importSessionIndex finds an Event's current Sessions by id or title.
type importSessionIndex struct {
	byID    map[uuid.UUID]ImportSession
	byTitle map[string][]ImportSession
}

func (x importSessionIndex) find(name string) (ImportSession, bool) {
	if id, err := uuid.Parse(name); err == nil {
		sess, ok := x.byID[id]
		return sess, ok
	}
	found := x.byTitle[strings.ToLower(collapseSpaces(name))]
	if len(found) != 1 {
		return ImportSession{}, false
	}
	return found[0], true
}

// importSessions are the Event's current Sessions, not cancelled.
func (s *service) importSessions(ctx context.Context, eventID uuid.UUID) (importSessionIndex, error) {
	x := importSessionIndex{byID: map[uuid.UUID]ImportSession{}, byTitle: map[string][]ImportSession{}}
	days, err := s.events.ListDays(ctx, eventID)
	if err != nil {
		return x, err
	}
	for _, d := range days {
		sessions, err := s.events.ListSessions(ctx, d.ID)
		if err != nil {
			return x, err
		}
		for _, sess := range sessions {
			if sess.Cancelled {
				continue
			}
			is := ImportSession{ID: sess.ID, Title: sess.Title}
			x.byID[sess.ID] = is
			key := strings.ToLower(collapseSpaces(sess.Title))
			x.byTitle[key] = append(x.byTitle[key], is)
		}
	}
	return x, nil
}

// finishImportAudit writes the import's outcome and counts to its row.
func (s *service) finishImportAudit(ctx context.Context, record *auditRecord, err error, sum ImportSummary, mail bool) {
	if record == nil {
		return
	}
	outcome, code := teamaudit.OutcomeOK, ""
	if err != nil {
		outcome, code = teamaudit.OutcomeFailed, "failed"
	}
	after := record.after
	after.Import = &teamaudit.ImportCounts{
		Rows: sum.Rows, Created: sum.Active, Waitlisted: sum.Waitlisted, Members: sum.Members, Guests: sum.Guests,
		Existing: sum.Existing, Full: sum.Full, Invalid: sum.Invalid, Duplicates: sum.Duplicates,
		Mail: mail,
	}
	if ferr := s.audit.Finish(ctx, record.id, outcome, code, &after); ferr != nil {
		s.logger.Printf("tickets: audit record %s stays pending", record.id)
	}
}

// rowRefused reports whether err is the database refusing one row's values
// (a data exception, class 22: a byte sequence it does not take, a value
// too long), not the database failing.
func rowRefused(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && strings.HasPrefix(pg.Code, "22")
}
