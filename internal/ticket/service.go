package ticket

import (
	"context"
	"errors"
	"log"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/doorqr"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/identity"
	"github.com/skylab-kulubu/core-backend/internal/teamaudit"
	"github.com/skylab-kulubu/core-backend/internal/ticketqr"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

type Service interface {
	Apply(ctx context.Context, p authz.Principal, eventID uuid.UUID) (Ticket, error)
	// ApplyJoining is Member apply that, with joinWaitlist, puts the member
	// on a full Event's waiting list (docs/event-capacity.md); without it a
	// full Event is ErrEventFull. Apply is ApplyJoining without it.
	ApplyJoining(ctx context.Context, p authz.Principal, eventID uuid.UUID, joinWaitlist bool) (Ticket, error)
	ApplyForOther(ctx context.Context, p authz.Principal, eventID, userID uuid.UUID) (Ticket, error)
	ListAssignableUsers(ctx context.Context, p authz.Principal, eventID uuid.UUID, query string) ([]PersonSummary, error)
	ListDoorEvents(ctx context.Context, p authz.Principal) ([]event.Resource, error)
	SearchDoorAttendees(ctx context.Context, p authz.Principal, eventID uuid.UUID, query string) ([]DoorAttendee, error)
	ApplyGuest(ctx context.Context, p authz.Principal, eventID uuid.UUID, g GuestInfo) (GuestApplication, error)
	// ApplyGuestJoining is Guest apply with the waiting list asked for, as
	// ApplyJoining; an operator of the Event adds past the capacity.
	ApplyGuestJoining(ctx context.Context, p authz.Principal, eventID uuid.UUID, g GuestInfo, joinWaitlist bool) (GuestApplication, error)
	// PromoteTicket is an operator's "Öne al": a waitlisted Ticket made
	// active, past the capacity if it must (docs/event-capacity.md).
	PromoteTicket(ctx context.Context, p authz.Principal, id uuid.UUID) (Ticket, error)
	RecordFormResponse(ctx context.Context, p authz.Principal, r FormResponse) (FormResponseResult, error)
	CanRecordFormResponse(p authz.Principal) bool
	// AdmitFormRespondent writes an Unticketed respondent's Ticket on the
	// Event (docs/form-respondents.md); the caller checked who may.
	AdmitFormRespondent(ctx context.Context, eventID uuid.UUID, r FormRespondent) (FormRespondentOutcome, error)
	// KeepFormRespondent applies an accepted answer to the person's Ticket
	// on the Event without writing one; EnsureFormRespondent makes sure core
	// has a signed-in person's row; BeginFormRespondentSend opens the
	// sending's Team audit row, FinishFormRespondentJob closes a send-all
	// job's when the job ended (docs/form-respondents.md).
	KeepFormRespondent(ctx context.Context, eventID uuid.UUID, r FormRespondent) error
	EnsureFormRespondent(ctx context.Context, userID uuid.UUID) (bool, error)
	BeginFormRespondentSend(ctx context.Context, p authz.Principal, eventID uuid.UUID) (*FormRespondentSend, error)
	FinishFormRespondentJob(ctx context.Context, eventID, jobID uuid.UUID, sum FormRespondentSummary, err error)
	Mine(ctx context.Context, p authz.Principal) ([]Ticket, error)
	// MineIncluding is the holder's active Tickets and those of the other
	// statuses include names (GET /v1/tickets/me?include=…).
	MineIncluding(ctx context.Context, p authz.Principal, include []Status) ([]Ticket, error)
	ListByEvent(ctx context.Context, p authz.Principal, eventID uuid.UUID) ([]Ticket, error)
	// ListByEventStatus is the panel's list of the Event's Tickets of the
	// statuses given, every status for none (GET /v1/events/{id}/tickets).
	ListByEventStatus(ctx context.Context, p authz.Principal, eventID uuid.UUID, statuses []Status) ([]Ticket, error)
	// CancelTicket and RestoreTicket are an operator's cancellation of a
	// Ticket and its taking back (docs/ticket-cancellation.md).
	CancelTicket(ctx context.Context, p authz.Principal, id uuid.UUID, reason string) (Ticket, error)
	// CancelTicketTelling is POST /v1/tickets/{id}/cancel: the Ticket's
	// holder cancels it themselves (kontenjan-iptal-hatirlatma 03; reason
	// and notify are not theirs to give, the holder is always told), an
	// operator as CancelTicket does, telling the holder when notify.
	CancelTicketTelling(ctx context.Context, p authz.Principal, id uuid.UUID, reason string, notify bool) (Ticket, error)
	RestoreTicket(ctx context.Context, p authz.Principal, id uuid.UUID) (Ticket, error)
	Get(ctx context.Context, p authz.Principal, id uuid.UUID) (Ticket, error)
	GetByUserEvent(ctx context.Context, p authz.Principal, userID, eventID uuid.UUID) (Ticket, error)
	ListQuery(ctx context.Context, p authz.Principal, email string, userID *uuid.UUID) ([]Ticket, error)
	CheckIn(ctx context.Context, p authz.Principal, ticketID, sessionID uuid.UUID) (CheckIn, error)
	// UndoCheckIn takes back the check-in ticketID has at sessionID: the
	// door desk's correction of a wrong one. Who may take check-ins at the
	// Event's door may, while the door takes them; ErrNotFound when the
	// Ticket has none there.
	UndoCheckIn(ctx context.Context, p authz.Principal, ticketID, sessionID uuid.UUID) error
	// UndoCheckInByID is UndoCheckIn by the check-in's own id, which the
	// door holds; ErrNotFound for a check-in of another Session.
	UndoCheckInByID(ctx context.Context, p authz.Principal, sessionID, checkInID uuid.UUID) error
	// CheckInMe is a member's own check-in by the Session QR. doorToken is
	// the scanned door or attendance QR's dq value; SESSION_QR_MODE=qr
	// requires it (docs/guest-self-check-in.md).
	CheckInMe(ctx context.Context, p authz.Principal, sessionID uuid.UUID, doorToken string) (CheckIn, error)
	CheckInGuest(ctx context.Context, sessionID uuid.UUID, g GuestCheckIn) (CheckIn, error)
	// MintDoorQR signs a door QR (doorqr.ClassDoor) or an attendance QR for a
	// projection screen (doorqr.ClassDisplay).
	MintDoorQR(ctx context.Context, p authz.Principal, sessionID uuid.UUID, class doorqr.Class) (DoorQR, error)
	CheckInUser(ctx context.Context, p authz.Principal, sessionID, userID uuid.UUID) (CheckIn, error)
	// AuthorizeSessionDoor answers whether p may take check-ins at the
	// Session's door (canUseDoor): ErrNotFound for no such Session,
	// ErrForbidden otherwise. A scanner route asks it before it reads (or
	// spends) a credential, so a refusal says nothing about the holder.
	AuthorizeSessionDoor(ctx context.Context, p authz.Principal, sessionID uuid.UUID) error
	ResolveAndCheckIn(ctx context.Context, p authz.Principal, sessionID uuid.UUID, target DoorCheckInTarget) (DoorCheckIn, error)
	DoorActivity(ctx context.Context, p authz.Principal, sessionID uuid.UUID) (DoorActivity, error)
	// SessionCheckIns is DoorActivity without the day's Venue entries: the
	// Session's Check-ins alone, in either mode (docs/venue-entry.md).
	SessionCheckIns(ctx context.Context, p authz.Principal, sessionID uuid.UUID) (DoorActivity, error)
	// CheckInTicketQR takes a check-in from a Ticket QR the door scanned
	// (docs/ticket-qr.md).
	CheckInTicketQR(ctx context.Context, p authz.Principal, sessionID uuid.UUID, token string) (CheckIn, error)
	// TicketQR is t's Ticket QR on ev, for its holder.
	TicketQR(t Ticket, ev event.Event) (string, error)
	// ResendTicket and RenewTicketLink are the panel's "Bileti yeniden
	// gönder" and "Bağlantıyı yenile" (docs/ticket-resend.md).
	ResendTicket(ctx context.Context, p authz.Principal, id uuid.UUID, requestKey string) (Resend, error)
	RenewTicketLink(ctx context.Context, p authz.Principal, id uuid.UUID, notify bool, requestKey string) (Ticket, Resend, error)
	EventDoor
	AttendeeImport
}

// EventDoor is the Event's door without a Session: Venue entry
// (docs/venue-entry.md). The writes ask what the Session door asks, in the
// same order, before any code is read.
type EventDoor interface {
	// AuthorizeEventDoor answers whether p may write entries at the Event's
	// door now: ErrNotFound, an *EventClosedError, or ErrForbidden.
	AuthorizeEventDoor(ctx context.Context, p authz.Principal, eventID uuid.UUID) error
	EnterTicketQR(ctx context.Context, p authz.Principal, eventID uuid.UUID, token string) (DoorEntry, error)
	EnterUser(ctx context.Context, p authz.Principal, eventID, userID uuid.UUID) (DoorEntry, error)
	ResolveEntry(ctx context.Context, p authz.Principal, eventID uuid.UUID, target DoorCheckInTarget) (DoorEntry, error)
	UndoEntry(ctx context.Context, p authz.Principal, eventID, entryID uuid.UUID) error
	EntrySummary(ctx context.Context, p authz.Principal, eventID uuid.UUID) (EntrySummary, error)
	// BeginExport reads what an attendee export of the Event needs for a
	// reader of its Tickets, and opens its Team audit row; FinishExport
	// closes the row with the file's row count (docs/attendee-export.md).
	BeginExport(ctx context.Context, p authz.Principal, eventID uuid.UUID, req ExportRequest) (*ExportData, error)
	FinishExport(ctx context.Context, data *ExportData, rows int, err error)
	ListEntries(ctx context.Context, p authz.Principal, eventID uuid.UUID, date string) ([]DoorEntry, error)
	// DoorStaffStats is the Event's door staff statistics
	// (docs/door-staff-stats.md): every row for a reader of its Tickets,
	// the caller's own for its door staff.
	DoorStaffStats(ctx context.Context, p authz.Principal, eventID uuid.UUID) (DoorStaffStats, error)
	// WalkIn is the door registering a person with no Ticket ("Kapıda
	// kayıt", docs/walk-in.md): their guest Ticket, and in the venue Entry
	// mode the day's entry. The ctx carries the caller for the Team audit
	// row (teamaudit.WithActor).
	WalkIn(ctx context.Context, p authz.Principal, eventID uuid.UUID, w WalkIn) (WalkInOutcome, error)
}

type TeamReader interface {
	GetGroup(ctx context.Context, idOrPath string) (identity.Group, error)
}

type PersonReader interface {
	GetUser(ctx context.Context, id uuid.UUID) (identity.Person, error)
}

type PersonDirectory interface {
	PersonReader
	SearchUsers(ctx context.Context, query string, limit int) ([]identity.Person, error)
}

type limitedUserSearcher interface {
	SearchLimit(ctx context.Context, query string, limit int) ([]user.User, error)
}

type service struct {
	tickets   Store
	events    event.Store
	users     user.Store
	people    PersonReader
	directory PersonDirectory
	teams     TeamReader
	authz     authz.Authorizer
	door      *doorqr.Gate
	// feedback marks who answered a feedback form (FeedbackAnswers); nil
	// marks nobody.
	feedback FeedbackAnswers
	// feedbackLinks reads the holder's feedback links for Mine; nil gives
	// none.
	feedbackLinks FeedbackLinks
	// logger writes what the service leaves out instead of failing (a
	// *log.Logger extra; log.Default() otherwise, as the handlers').
	logger *log.Logger
	// qr mints and verifies Ticket QRs (a *ticketqr.Signer extra); nil
	// refuses every Ticket QR.
	qr TicketQRs
	// scans counts Ticket QR scans (a *DoorScanMetrics extra); nil counts
	// none.
	scans *DoorScanMetrics
	// clock is now for the venue day and the Event's door (a Clock
	// extra); time.Now without one.
	clock Clock
	// page is the ticket page's settings (a PageConfig extra): the
	// holder's own list links each Ticket's page.
	page PageConfig
	// audit records the ticket actions in the Team audit record (a
	// teamaudit.Writer extra); nil records none.
	audit teamaudit.Writer
	// guestOwners finds the account a new guest Ticket's address is (a
	// GuestOwners extra); nil links none (docs/guest-account-links.md).
	guestOwners GuestOwners
}

// GuestOwners finds the account a new guest Ticket on the Event is linked
// to (docs/guest-account-links.md): the one active account that holds the
// address verified and has no Ticket on the Event, while linking is on.
// false links nothing; it never fails a write.
type GuestOwners interface {
	GuestOwner(ctx context.Context, eventID uuid.UUID, email string) (uuid.UUID, bool)
}

func NewService(tickets Store, events event.Store, az authz.Authorizer, extras ...any) Service {
	s := &service{tickets: tickets, events: events, authz: az}
	for _, extra := range extras {
		if d, ok := extra.(identity.Directory); ok {
			s.people = d
			s.directory = d
			s.teams = d
		}
		if v, ok := extra.(user.Store); ok {
			s.users = v
		}
		if v, ok := extra.(TeamReader); ok && s.teams == nil {
			s.teams = v
		}
		if v, ok := extra.(PersonReader); ok && s.people == nil {
			s.people = v
		}
		if v, ok := extra.(*doorqr.Gate); ok && v != nil {
			s.door = v
		}
		if v, ok := extra.(FeedbackAnswers); ok && v != nil {
			s.feedback = v
		}
		if v, ok := extra.(FeedbackLinks); ok && v != nil {
			s.feedbackLinks = v
		}
		if v, ok := extra.(*log.Logger); ok && v != nil {
			s.logger = v
		}
		if v, ok := extra.(*ticketqr.Signer); ok && v != nil {
			s.qr = v
		}
		if v, ok := extra.(*DoorScanMetrics); ok && v != nil {
			s.scans = v
		}
		if v, ok := extra.(Clock); ok && v != nil {
			s.clock = v
		}
		if v, ok := extra.(PageConfig); ok {
			s.page = v
		}
		if v, ok := extra.(teamaudit.Writer); ok && v != nil {
			s.audit = v
		}
		if v, ok := extra.(GuestOwners); ok && v != nil {
			s.guestOwners = v
		}
	}
	// The in-memory store reads its Events' capacity as the database does.
	if m, ok := tickets.(*MemoryStore); ok && events != nil {
		m.UseEvents(events)
	}
	if s.door == nil {
		s.door = doorqr.NewEphemeralGate()
	}
	if s.logger == nil {
		s.logger = log.Default()
	}
	return s
}

const assignableUserLimit = 20

func (s *service) personSummary(ctx context.Context, person identity.Person, found bool) (PersonSummary, bool) {
	if s.users != nil {
		if shadow, err := s.users.Get(ctx, person.ID); err == nil {
			found = true
			if shadow.Email != "" {
				person.Email = shadow.Email
			}
			person.FirstName = shadow.FirstName
			person.LastName = shadow.LastName
		}
	}
	return PersonSummary{ID: person.ID, Email: person.Email, FirstName: person.FirstName, LastName: person.LastName}, found
}

func (s *service) ListAssignableUsers(ctx context.Context, p authz.Principal, eventID uuid.UUID, query string) ([]PersonSummary, error) {
	ev, err := s.events.Get(ctx, eventID)
	if err != nil {
		if errors.Is(err, event.ErrNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if err := s.seen(&p, ev); err != nil {
		return nil, err
	}
	if !s.authz.Allow(p, eventResource(ev, authz.TypeTicket), authz.Assign) {
		return nil, ErrForbidden
	}
	query = strings.TrimSpace(query)
	if len([]rune(query)) < 2 {
		return []PersonSummary{}, nil
	}
	if s.directory == nil && s.users == nil {
		return []PersonSummary{}, nil
	}
	people := make([]identity.Person, 0, assignableUserLimit)
	var directoryErr error
	if s.directory != nil {
		people, directoryErr = s.directory.SearchUsers(ctx, query, assignableUserLimit)
	}
	byID := make(map[uuid.UUID]PersonSummary)
	for _, person := range people {
		summary, _ := s.personSummary(ctx, person, true)
		if personSummaryMatches(summary, query) {
			byID[summary.ID] = summary
		}
	}
	var shadowErr error
	if s.users != nil {
		var shadows []user.User
		if searcher, ok := s.users.(limitedUserSearcher); ok {
			shadows, shadowErr = searcher.SearchLimit(ctx, query, assignableUserLimit)
		} else {
			shadows, shadowErr = s.users.Search(ctx, query)
		}
		for _, shadow := range shadows {
			summary := PersonSummary{
				ID: shadow.ID, Email: shadow.Email, FirstName: shadow.FirstName, LastName: shadow.LastName,
			}
			if personSummaryMatches(summary, query) {
				byID[shadow.ID] = summary
			}
		}
	}
	if directoryErr != nil && shadowErr != nil {
		return nil, directoryErr
	}
	if directoryErr != nil && s.users == nil {
		return nil, directoryErr
	}
	if shadowErr != nil && s.directory == nil {
		return nil, shadowErr
	}
	out := make([]PersonSummary, 0, len(byID))
	for _, summary := range byID {
		out = append(out, summary)
	}
	sort.Slice(out, func(i, j int) bool {
		left := strings.ToLower(out[i].LastName + "\x00" + out[i].FirstName + "\x00" + out[i].Email)
		right := strings.ToLower(out[j].LastName + "\x00" + out[j].FirstName + "\x00" + out[j].Email)
		return left < right
	})
	if len(out) > assignableUserLimit {
		out = out[:assignableUserLimit]
	}
	return out, nil
}

func personSummaryMatches(summary PersonSummary, query string) bool {
	needle := strings.ToLower(strings.TrimSpace(query))
	if needle == "" {
		return false
	}
	values := []string{
		summary.Email,
		summary.FirstName,
		summary.LastName,
		strings.TrimSpace(summary.FirstName + " " + summary.LastName),
	}
	for _, value := range values {
		if strings.Contains(strings.ToLower(value), needle) {
			return true
		}
	}
	return false
}

// withEvent gives t its Event and, for a member's Ticket, its owner's
// summary. A guest Ticket linked to an account (docs/guest-account-links.md)
// keeps its ownerId but gets no summary: it is the guest's name and e-mail
// that the Ticket was written for, and the account's own name and
// addresses are not the reader's to learn from it.
func (s *service) withEvent(ctx context.Context, t Ticket) Ticket {
	if t.OwnerID != nil && t.TicketType != Guest {
		if s.users != nil {
			if shadow, err := s.users.Get(ctx, *t.OwnerID); err == nil {
				summary := ownerSummary(shadow)
				t.Owner = &summary
			}
		}
		if t.Owner == nil && s.people != nil {
			if directoryPerson, err := s.people.GetUser(ctx, *t.OwnerID); err == nil {
				summary := activeSummary(PersonSummary{
					ID: directoryPerson.ID, Email: directoryPerson.Email,
					FirstName: directoryPerson.FirstName, LastName: directoryPerson.LastName,
				})
				t.Owner = &summary
			}
		}
	}
	ev, err := s.events.Get(ctx, t.EventID)
	if err != nil {
		return t
	}
	res := ev.Resource()
	t.Event = &res
	return t
}

func (s *service) withEvents(ctx context.Context, tickets []Ticket) []Ticket {
	out := make([]Ticket, len(tickets))
	for i, t := range tickets {
		out[i] = s.withEvent(ctx, t)
	}
	return out
}

// ListDoorEvents lists no draft: events.List leaves them out.
func (s *service) ListDoorEvents(ctx context.Context, p authz.Principal) ([]event.Resource, error) {
	events, err := s.events.List(ctx, "", false)
	if err != nil {
		return nil, err
	}
	out := make([]event.Resource, 0)
	for _, ev := range events {
		// A cancelled Event's door is closed: the door apps leave it out.
		if ev.Status == event.StatusPublished && s.canUseDoor(ctx, p, ev) {
			out = append(out, ev.Resource())
		}
	}
	return out, nil
}

func valuesContainDoorQuery(values []string, query string) bool {
	needle := normalizedDoorValue(query)
	if needle == "" {
		return false
	}
	for _, value := range values {
		if strings.Contains(normalizedDoorValue(value), needle) {
			return true
		}
	}
	return false
}

func (s *service) SearchDoorAttendees(
	ctx context.Context,
	p authz.Principal,
	eventID uuid.UUID,
	query string,
) ([]DoorAttendee, error) {
	ev, err := s.events.Get(ctx, eventID)
	if err != nil {
		if errors.Is(err, event.ErrNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if err := s.seen(&p, ev); err != nil {
		return nil, err
	}
	if !s.canUseDoor(ctx, p, ev) {
		return nil, ErrForbidden
	}
	query = strings.TrimSpace(query)
	if len([]rune(query)) < 2 {
		return []DoorAttendee{}, nil
	}
	// A Ticket's code finds that Ticket alone (docs/ticket-qr.md).
	if row, found, err := s.doorTicketByCode(ctx, eventID, query); err != nil {
		return nil, err
	} else if found {
		// A Ticket that is not active is no attendee: the code finds
		// nobody else either.
		if !row.Ticket.Active() {
			return []DoorAttendee{}, nil
		}
		attendee := DoorAttendee{Name: row.Name, Email: row.Email}
		if row.Ticket.OwnerID != nil {
			personID := *row.Ticket.OwnerID
			attendee.PersonID = &personID
		}
		return []DoorAttendee{attendee}, nil
	}
	if store, ok := s.tickets.(DoorStore); ok {
		return s.searchStoredDoorAttendees(ctx, store, eventID, query)
	}
	listed, err := s.tickets.ListByEvent(ctx, eventID)
	if err != nil {
		return nil, err
	}
	out := make([]DoorAttendee, 0)
	for _, candidate := range listed {
		name, values, found := s.localDoorTicketIdentity(ctx, candidate)
		email := ""
		if candidate.TicketType == Guest {
			email = candidate.GuestEmail
		} else if candidate.OwnerID != nil {
			if found && len(values) > 0 {
				email = values[0]
			}
		}
		if !found || !valuesContainDoorQuery(values, query) {
			continue
		}
		row := DoorAttendee{Name: name, Email: email}
		if candidate.OwnerID != nil {
			personID := *candidate.OwnerID
			row.PersonID = &personID
		}
		out = append(out, row)
	}
	if len(out) == 0 && s.directory != nil {
		people, _ := s.directory.SearchUsers(ctx, query, assignableUserLimit)
		directoryMatches := make(map[uuid.UUID]PersonSummary)
		for _, person := range people {
			summary, _ := s.personSummary(ctx, person, true)
			_, values := summaryDoorIdentity(summary)
			if valuesContainDoorQuery(values, query) {
				directoryMatches[summary.ID] = summary
			}
		}
		for _, candidate := range listed {
			if candidate.OwnerID == nil {
				continue
			}
			summary, ok := directoryMatches[*candidate.OwnerID]
			if !ok {
				continue
			}
			name, _ := summaryDoorIdentity(summary)
			personID := *candidate.OwnerID
			out = append(out, DoorAttendee{
				PersonID: &personID, Name: name, Email: summary.Email,
			})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return strings.ToLower(out[i].Name+"\x00"+out[i].Email) < strings.ToLower(out[j].Name+"\x00"+out[j].Email)
	})
	if len(out) > assignableUserLimit {
		out = out[:assignableUserLimit]
	}
	return out, nil
}

func (s *service) searchStoredDoorAttendees(
	ctx context.Context,
	store DoorStore,
	eventID uuid.UUID,
	query string,
) ([]DoorAttendee, error) {
	rows, err := store.SearchDoorTickets(ctx, eventID, query, false, assignableUserLimit)
	if err != nil {
		return nil, err
	}
	out := make([]DoorAttendee, 0, len(rows))
	for _, candidate := range rows {
		if !candidate.Ticket.Active() {
			continue
		}
		row := DoorAttendee{Name: candidate.Name, Email: candidate.Email}
		if candidate.Ticket.OwnerID != nil {
			personID := *candidate.Ticket.OwnerID
			row.PersonID = &personID
		}
		out = append(out, row)
	}
	if len(out) > 0 || s.directory == nil {
		return out, nil
	}
	people, _ := s.directory.SearchUsers(ctx, query, assignableUserLimit)
	directoryMatches := make(map[uuid.UUID]PersonSummary)
	ownerIDs := make([]uuid.UUID, 0, len(people))
	for _, person := range people {
		summary := PersonSummary{
			ID: person.ID, Email: person.Email, FirstName: person.FirstName, LastName: person.LastName,
		}
		directoryMatches[summary.ID] = summary
		ownerIDs = append(ownerIDs, summary.ID)
	}
	tickets, err := store.DoorTicketsByOwners(ctx, eventID, ownerIDs)
	if err != nil {
		return nil, err
	}
	for _, candidate := range tickets {
		if candidate.Ticket.OwnerID == nil || !candidate.Ticket.Active() {
			continue
		}
		summary, found := directoryMatches[*candidate.Ticket.OwnerID]
		if !found {
			continue
		}
		name, email := candidate.Name, candidate.Email
		if candidate.Found {
			if !valuesContainDoorQuery([]string{email, name, candidate.Ticket.OwnerID.String()}, query) {
				continue
			}
		} else {
			var values []string
			name, values = summaryDoorIdentity(summary)
			if !valuesContainDoorQuery(values, query) {
				continue
			}
			email = summary.Email
		}
		personID := *candidate.Ticket.OwnerID
		out = append(out, DoorAttendee{PersonID: &personID, Name: name, Email: email})
	}
	sort.Slice(out, func(i, j int) bool {
		return strings.ToLower(out[i].Name+"\x00"+out[i].Email) < strings.ToLower(out[j].Name+"\x00"+out[j].Email)
	})
	if len(out) > assignableUserLimit {
		out = out[:assignableUserLimit]
	}
	return out, nil
}

func (s *service) canRead(ctx context.Context, p authz.Principal, t Ticket) bool {
	return s.readable(ctx, p, t) == nil
}

// readable is nil when p may read t: their own Ticket always (its holder sees
// a draft's summary, decision of 2026-10-19), anyone else's only on an Event
// p may see, ErrNotFound on a draft p may not see (ADR 0066) and
// ErrForbidden without the Ticket read right.
func (s *service) readable(ctx context.Context, p authz.Principal, t Ticket) error {
	if t.OwnerID != nil && p.ID == t.OwnerID.String() {
		return nil
	}
	ev, err := s.events.Get(ctx, t.EventID)
	if err != nil {
		return ErrForbidden
	}
	if err := s.seen(&p, ev); err != nil {
		return err
	}
	if !s.authz.Allow(p, eventResource(ev, authz.TypeTicket), authz.Read) {
		return ErrForbidden
	}
	return nil
}

func (s *service) Apply(ctx context.Context, p authz.Principal, eventID uuid.UUID) (Ticket, error) {
	return s.ApplyJoining(ctx, p, eventID, false)
}

func (s *service) ApplyJoining(ctx context.Context, p authz.Principal, eventID uuid.UUID, joinWaitlist bool) (Ticket, error) {
	if !s.authz.Allow(p, authz.Resource{Type: authz.TypeTicket}, authz.Create) {
		return Ticket{}, ErrForbidden
	}
	ev, err := s.events.Get(ctx, eventID)
	if err != nil {
		if errors.Is(err, event.ErrNotFound) {
			return Ticket{}, ErrNotFound
		}
		return Ticket{}, err
	}
	if err := s.openForRegistration(&p, ev, false); err != nil {
		return Ticket{}, err
	}
	ownerID, err := uuid.Parse(p.ID)
	if err != nil {
		return Ticket{}, invalid(CodeTokenSubjectInvalid)
	}
	return s.applyRegistered(ctx, eventID, ownerID, Admission{Waitlist: joinWaitlist})
}

func (s *service) ApplyForOther(ctx context.Context, p authz.Principal, eventID, userID uuid.UUID) (Ticket, error) {
	ev, err := s.events.Get(ctx, eventID)
	if err != nil {
		if errors.Is(err, event.ErrNotFound) {
			return Ticket{}, ErrNotFound
		}
		return Ticket{}, err
	}
	// Adding a person from the panel works on a draft too (speakers, the
	// team): only for who may see it. A cancelled Event takes nobody new.
	if err := s.seen(&p, ev); err != nil {
		return Ticket{}, err
	}
	if !s.authz.Allow(p, eventResource(ev, authz.TypeTicket), authz.Assign) {
		return Ticket{}, ErrForbidden
	}
	if ev.Status == event.StatusCancelled {
		return Ticket{}, ErrEventCancelled
	}
	if userID == uuid.Nil {
		return Ticket{}, invalid(CodeUserIDInvalid)
	}
	if err := s.resolveApplyTarget(ctx, userID); err != nil {
		return Ticket{}, err
	}
	// An operator adds past the capacity (Yusuf, 2026-11-24): the answer's
	// overCapacity tells the panel to warn.
	return s.applyRegistered(ctx, eventID, userID, Admission{Over: true})
}

func (s *service) resolveApplyTarget(ctx context.Context, userID uuid.UUID) error {
	if s.users != nil {
		if _, err := s.users.Get(ctx, userID); err == nil {
			return nil
		} else if !errors.Is(err, user.ErrNotFound) {
			return err
		}
	}
	people := s.people
	if people == nil {
		if p, ok := any(s.teams).(PersonReader); ok {
			people = p
		}
	}
	if people == nil {
		return ErrNotFound
	}
	person, err := people.GetUser(ctx, userID)
	if err != nil {
		if errors.Is(err, identity.ErrNotFound) {
			return ErrNotFound
		}
		return err
	}
	if s.users == nil {
		return nil
	}
	_, _, err = user.NewService(s.users).Ensure(ctx, userID, user.Profile{
		Email:       person.Email,
		FirstName:   person.FirstName,
		LastName:    person.LastName,
		Username:    person.Username,
		SchoolEmail: person.SchoolEmail,
		SkyNumber:   person.SkyNumber,
	})
	return err
}

// applyRegistered writes the member's Ticket on the Event under its
// capacity (a). One already there is ErrConflict, unless it is cancelled:
// the member registering again reopens it (new link and QR, its mail again;
// reopenIfCancelled). A waitlisted one is ErrTicketWaitlisted (409
// ticket_waitlisted: an older sky-app must not read it as "already
// registered"), unless an operator adds the person (a.Over): then it is
// promoted, past the capacity if it must.
func (s *service) applyRegistered(ctx context.Context, eventID, ownerID uuid.UUID, a Admission) (Ticket, error) {
	existing, err := s.tickets.GetByOwnerEvent(ctx, ownerID, eventID)
	switch {
	case err == nil:
		if existing.StatusOf() == StatusWaitlisted {
			if !a.Over {
				return Ticket{}, ErrTicketWaitlisted
			}
			promoted, err := s.promoteWaiting(ctx, existing)
			if err != nil {
				return Ticket{}, err
			}
			return s.withEvent(ctx, promoted), nil
		}
		reopened, changed, err := s.reopenIfCancelled(ctx, existing, a)
		if err != nil {
			return Ticket{}, err
		}
		if !changed {
			return Ticket{}, ErrConflict
		}
		return s.withEvent(ctx, reopened), nil
	case !errors.Is(err, ErrNotFound):
		return Ticket{}, err
	}
	created, err := s.create(ctx, Ticket{
		EventID:    eventID,
		TicketType: Registered,
		OwnerID:    &ownerID,
	}, a)
	if err != nil {
		return Ticket{}, err
	}
	return s.withEvent(ctx, created), nil
}

// promoteWaiting moves a waitlisted Ticket to active for an operator
// adding its holder from the panel ("Katılımcı ekle" on who waits): as
// "Öne al", past the capacity if it must, its mail queued.
func (s *service) promoteWaiting(ctx context.Context, t Ticket) (Ticket, error) {
	store, err := s.capacityStore()
	if err != nil {
		return Ticket{}, err
	}
	promoted, _, err := store.Promote(ctx, t.ID)
	return promoted, err
}

// create writes t under its Event's capacity (a) on a CapacityStore, as
// Create on any other.
func (s *service) create(ctx context.Context, t Ticket, a Admission) (Ticket, error) {
	if store, ok := s.tickets.(CapacityStore); ok {
		return store.CreateAdmitted(ctx, t, a)
	}
	return s.tickets.Create(ctx, t)
}

// withGuestOwner is t, a new guest Ticket, with the account its address is
// as its owner (GuestOwners), when there is one: the Ticket stays GUEST with
// its name and e-mail (docs/guest-account-links.md).
func (s *service) withGuestOwner(ctx context.Context, t Ticket) Ticket {
	if s.guestOwners == nil || t.TicketType != Guest || t.OwnerID != nil || strings.TrimSpace(t.GuestEmail) == "" {
		return t
	}
	owner, ok := s.guestOwners.GuestOwner(ctx, t.EventID, t.GuestEmail)
	if !ok {
		return t
	}
	t.OwnerID = &owner
	t.ownerLink = LinkWrite
	return t
}

// createGuest writes the new guest Ticket t with write, linked to the
// account its address is when there is one. A link the database refuses
// (the account took a Ticket on the Event or stopped being active since it
// was looked up) writes it unlinked, as before linking existed: linking
// never fails a write.
func (s *service) createGuest(ctx context.Context, t Ticket, write func(Ticket) (Ticket, error)) (Ticket, error) {
	linked := s.withGuestOwner(ctx, t)
	if linked.OwnerID == nil {
		return write(t)
	}
	created, err := write(linked)
	if errors.Is(err, ErrConflict) || errors.Is(err, ErrNotFound) {
		return write(t)
	}
	return created, err
}

// missingGuestFields names the fields of a normalized g a guest
// application needs and lacks, in the body's order.
func missingGuestFields(g GuestInfo) []string {
	missing := []string{}
	if g.FirstName == "" {
		missing = append(missing, "firstName")
	}
	if g.LastName == "" {
		missing = append(missing, "lastName")
	}
	if g.Email == "" {
		missing = append(missing, "email")
	}
	return missing
}

func normalizeGuest(g GuestInfo) GuestInfo {
	g.FirstName = strings.TrimSpace(g.FirstName)
	g.LastName = strings.TrimSpace(g.LastName)
	g.Email = strings.ToLower(strings.TrimSpace(g.Email))
	g.PhoneNumber = strings.TrimSpace(g.PhoneNumber)
	g.University = strings.TrimSpace(g.University)
	g.Department = strings.TrimSpace(g.Department)
	g.Grade = strings.TrimSpace(g.Grade)
	// The details are optional: one the columns would refuse is left out,
	// never the guest (a form's answer is a stranger's text).
	if utf8.RuneCountInString(g.University) > MaxWalkInText {
		g.University = ""
	}
	if utf8.RuneCountInString(g.Department) > MaxWalkInText {
		g.Department = ""
	}
	if g.Grade != "" && !slices.Contains(Grades, g.Grade) {
		g.Grade = ""
	}
	return g
}

// ApplyGuest writes the guest Ticket of g's e-mail on the Event, or finds the
// one already there (docs/guest-apply.md).
//
// A token holder must be an operator of the Event (guestApplyTrusted), else
// ErrForbidden, a service account too: with sign-up open a person's token
// costs no more than an e-mail address, and a guest Ticket takes a place and
// mails a stranger. An operator gets the Ticket back and may change an existing guest's name or phone
// number. The token-less internal hop (the route answers it only off the
// edge) gets the zero Ticket and may only fill a detail the stored Ticket
// lacks: whoever knows a guest's e-mail and a public Event id must not read
// the guest's phone number or rename them.
func (s *service) ApplyGuest(ctx context.Context, p authz.Principal, eventID uuid.UUID, g GuestInfo) (GuestApplication, error) {
	return s.ApplyGuestJoining(ctx, p, eventID, g, false)
}

// ApplyGuestJoining is ApplyGuest under the Event's capacity: an operator of
// the Event adds past it, anybody else joins the waiting list of a full
// Event only with joinWaitlist (else ErrEventFull). The untrusted answer
// (Waitlisted) is told from the Event, not from a Ticket already there, so
// it says no more than before about whether the e-mail had applied.
func (s *service) ApplyGuestJoining(ctx context.Context, p authz.Principal, eventID uuid.UUID, g GuestInfo, joinWaitlist bool) (GuestApplication, error) {
	g = normalizeGuest(g)
	if missing := missingGuestFields(g); len(missing) > 0 {
		return GuestApplication{}, invalidWith(CodeGuestFieldsRequired, map[string]any{"fields": missing})
	}
	ev, err := s.events.Get(ctx, eventID)
	if err != nil {
		if errors.Is(err, event.ErrNotFound) {
			return GuestApplication{}, ErrNotFound
		}
		return GuestApplication{}, err
	}
	operator := s.operatorOf(p, ev)
	// On a draft the route is the panel's alone (K5, 2026-11-22): who may
	// not see it gets 404, who sees it but may not add people (DK) 403, as
	// adding a person: rights first, so the draft's state is told to no
	// one who may not act on it.
	if ev.Status == event.StatusDraft {
		if err := s.seen(&p, ev); err != nil {
			return GuestApplication{}, err
		}
		if !operator {
			return GuestApplication{}, ErrForbidden
		}
	}
	trusted := s.guestApplyTrusted(p, ev)
	if p.ID != "" && !trusted {
		return GuestApplication{}, ErrForbidden
	}
	if err := s.openForRegistration(&p, ev, operator); err != nil {
		return GuestApplication{}, err
	}
	a := Admission{Waitlist: joinWaitlist}
	if operator {
		a = Admission{Over: true}
	}
	applied, err := s.writeGuest(ctx, eventID, g, trusted, a)
	if err != nil {
		return GuestApplication{}, err
	}
	applied.Trusted = trusted
	if trusted {
		applied.Ticket = s.withEvent(ctx, applied.Ticket)
		applied.Waitlisted = applied.Ticket.StatusOf() == StatusWaitlisted
		return applied, nil
	}
	if applied.Result == GuestCreated {
		applied.Waitlisted = applied.Ticket.StatusOf() == StatusWaitlisted
	} else if applied.Waitlisted, err = s.untrustedWaitlisted(ctx, eventID, a); err != nil {
		return GuestApplication{}, err
	}
	applied.Ticket = Ticket{}
	return applied, nil
}

// untrustedWaitlisted is the answer to an untrusted Guest apply that found
// the Ticket already there: as a new guest's would be on the Event now,
// waitlisted or refused when it is full, whatever the Ticket's status.
func (s *service) untrustedWaitlisted(ctx context.Context, eventID uuid.UUID, a Admission) (bool, error) {
	store, ok := s.tickets.(CapacityStore)
	if !ok {
		return false, nil
	}
	c, err := store.Capacity(ctx, eventID)
	if err != nil {
		return false, err
	}
	status, _, err := admit(c, a)
	return status == StatusWaitlisted, err
}

// operatorOf reports whether p is a person who runs ev from the panel: who
// may update the Event or assign its Tickets, the people "Katılımcı ekle"
// is offered to. A product's service identity is not one.
func (s *service) operatorOf(p authz.Principal, ev event.Event) bool {
	return p.Product == "" && p.ID != "" &&
		(s.authz.Allow(p, eventResource(ev, authz.TypeEvent), authz.Update) ||
			s.authz.Allow(p, eventResource(ev, authz.TypeTicket), authz.Assign))
}

// guestApplyTrusted reports whether p may write a guest with a token, see the
// Ticket a Guest apply wrote and change an existing guest's details: an
// operator of the Event (operatorOf), the people the Event hub offers
// "Katılımcı ekle" to. No service account: the forms service reports its
// answers instead (docs/form-response-tickets.md).
func (s *service) guestApplyTrusted(p authz.Principal, ev event.Event) bool {
	return s.operatorOf(p, ev)
}

func (s *service) writeGuest(ctx context.Context, eventID uuid.UUID, g GuestInfo, trusted bool, a Admission) (GuestApplication, error) {
	existing, err := s.tickets.GetByGuestEvent(ctx, g.Email, eventID)
	if errors.Is(err, ErrNotFound) {
		created, createErr := s.createGuest(ctx, Ticket{
			EventID:          eventID,
			TicketType:       Guest,
			GuestFirstName:   g.FirstName,
			GuestLastName:    g.LastName,
			GuestEmail:       g.Email,
			GuestPhoneNumber: g.PhoneNumber,
			GuestUniversity:  g.University,
			GuestDepartment:  g.Department,
			GuestGrade:       g.Grade,
		}, func(t Ticket) (Ticket, error) { return s.create(ctx, t, a) })
		if createErr == nil {
			return GuestApplication{Ticket: created, Result: GuestCreated}, nil
		}
		if !errors.Is(createErr, ErrConflict) {
			return GuestApplication{}, createErr
		}
		// A concurrent application (a double submit) wrote the Ticket since
		// the read: answer as for one that was already there.
		existing, err = s.tickets.GetByGuestEvent(ctx, g.Email, eventID)
	}
	if err != nil {
		return GuestApplication{}, err
	}
	// A waitlisted guest added from the panel is promoted, as a member is
	// (applyRegistered).
	if a.Over && existing.StatusOf() == StatusWaitlisted {
		if existing, err = s.promoteWaiting(ctx, existing); err != nil {
			return GuestApplication{}, err
		}
	}
	// A cancelled guest Ticket registered again is reopened: written anew,
	// as far as the caller and the forms report are concerned.
	existing, reopened, err := s.reopenIfCancelled(ctx, existing, a)
	if err != nil {
		return GuestApplication{}, err
	}
	if trusted {
		existing.GuestFirstName = g.FirstName
		existing.GuestLastName = g.LastName
		existing.GuestEmail = g.Email
		if g.PhoneNumber != "" {
			existing.GuestPhoneNumber = g.PhoneNumber
		}
		for _, field := range []struct {
			stored *string
			sent   string
		}{
			{&existing.GuestUniversity, g.University},
			{&existing.GuestDepartment, g.Department},
			{&existing.GuestGrade, g.Grade},
		} {
			if field.sent != "" {
				*field.stored = field.sent
			}
		}
		updated, err := s.tickets.Update(ctx, existing)
		if err != nil {
			return GuestApplication{}, err
		}
		if reopened {
			return GuestApplication{Ticket: updated, Result: GuestCreated}, nil
		}
		return GuestApplication{Ticket: updated, Result: GuestExisting}, nil
	}
	result := GuestExisting
	filled := false
	for _, field := range []struct {
		stored *string
		sent   string
	}{
		{&existing.GuestFirstName, g.FirstName},
		{&existing.GuestLastName, g.LastName},
		{&existing.GuestPhoneNumber, g.PhoneNumber},
		{&existing.GuestUniversity, g.University},
		{&existing.GuestDepartment, g.Department},
		{&existing.GuestGrade, g.Grade},
	} {
		switch {
		case field.sent == "" || field.sent == *field.stored:
			// Nothing asked for.
		case *field.stored == "":
			*field.stored = field.sent
			filled = true
		default:
			result = GuestKept
		}
	}
	if reopened {
		result = GuestCreated
	}
	if !filled {
		return GuestApplication{Ticket: existing, Result: result}, nil
	}
	updated, err := s.tickets.Update(ctx, existing)
	if err != nil {
		return GuestApplication{}, err
	}
	return GuestApplication{Ticket: updated, Result: result}, nil
}

// Mine is the holder's active Tickets: an older client (sky-app) that knows
// no status sees no cancelled Ticket.
func (s *service) Mine(ctx context.Context, p authz.Principal) ([]Ticket, error) {
	return s.MineIncluding(ctx, p, nil)
}

func (s *service) MineIncluding(ctx context.Context, p authz.Principal, include []Status) ([]Ticket, error) {
	if !s.authz.Allow(p, authz.Resource{Type: authz.TypeTicket}, authz.ReadMe) {
		return nil, ErrForbidden
	}
	ownerID, err := uuid.Parse(p.ID)
	if err != nil {
		return nil, invalid(CodeTokenSubjectInvalid)
	}
	tickets, err := s.tickets.ListByOwner(ctx, ownerID)
	if err != nil {
		return nil, err
	}
	tickets = onlyStatuses(tickets, append([]Status{StatusActive}, include...))
	return s.withCancellable(ctx, s.withHolderLinks(ctx, s.withFeedbackLinks(ctx, s.withEvents(ctx, tickets)))), nil
}

// withCancellable says on each of the holder's Tickets whether they may
// cancel it now (HolderMayCancel), the check-ins and Entrants read in one
// query for all of them. When those cannot be read the Tickets say false
// (the button hides; the route still decides), and a log line naming
// nobody says so.
func (s *service) withCancellable(ctx context.Context, tickets []Ticket) []Ticket {
	if len(tickets) == 0 {
		return tickets
	}
	blocked := map[uuid.UUID]bool{}
	unread := false
	if store, ok := s.tickets.(CancelBlockStore); ok {
		ids := make([]uuid.UUID, len(tickets))
		for i, t := range tickets {
			ids[i] = t.ID
		}
		got, err := store.CancelBlocked(ctx, ids)
		if err != nil {
			s.logger.Printf("tickets: cancellation blocks not read, %d tickets answered not cancellable: %v", len(tickets), err)
			unread = true
		} else {
			blocked = got
		}
	}
	now := time.Now()
	events := map[uuid.UUID]*event.Event{}
	for i := range tickets {
		t := &tickets[i]
		ev, seen := events[t.EventID]
		if !seen {
			if got, err := s.events.Get(ctx, t.EventID); err == nil {
				ev = &got
			}
			events[t.EventID] = ev
		}
		may := !unread && ev != nil && HolderMayCancel(*ev, *t, now, blocked[t.ID])
		t.Cancellable = &may
	}
	return tickets
}

// withHolderLinks gives the holder's own Tickets their ticket page
// (ticketUrl, while TICKET_PAGE_URL is set) and their Ticket QR (qrToken,
// on a published Event): not on a draft, nor once the page has closed
// (docs/ticket-page.md). A QR that cannot be signed is left out.
func (s *service) withHolderLinks(ctx context.Context, tickets []Ticket) []Ticket {
	now := time.Now()
	events := map[uuid.UUID]*event.Event{}
	for i := range tickets {
		t := &tickets[i]
		if !held(*t) || t.AccessKey == "" {
			continue
		}
		ev, seen := events[t.EventID]
		if !seen {
			if got, err := s.events.Get(ctx, t.EventID); err == nil {
				ev = &got
			}
			events[t.EventID] = ev
		}
		if ev == nil || ev.Status == event.StatusDraft || s.page.Closed(*ev, now) {
			continue
		}
		// The page tells a cancelled or waitlisted Ticket so; only an
		// active one has a QR.
		t.TicketURL = s.page.Link(t.AccessKey)
		if ev.Status == event.StatusPublished && s.qr != nil && t.Active() {
			if token, err := s.TicketQR(*t, *ev); err == nil {
				t.QRToken = token
			}
		}
	}
	return tickets
}

// withFeedbackLinks gives the holder's Tickets their feedback links, read in
// one query for all of them. The links are an extra: when they cannot be
// read the Tickets go without them, and a log line (naming nobody) says so.
func (s *service) withFeedbackLinks(ctx context.Context, tickets []Ticket) []Ticket {
	if s.feedbackLinks == nil || len(tickets) == 0 {
		return tickets
	}
	ids := make([]uuid.UUID, len(tickets))
	for i, t := range tickets {
		ids[i] = t.ID
	}
	links, err := s.feedbackLinks.TicketLinks(ctx, ids, time.Now().UTC())
	if err != nil {
		s.logger.Printf("tickets: feedback links not read, %d tickets answered without them: %v", len(tickets), err)
		return tickets
	}
	for i := range tickets {
		tickets[i].Feedback = links[tickets[i].ID]
	}
	return tickets
}

func (s *service) ListByEvent(ctx context.Context, p authz.Principal, eventID uuid.UUID) ([]Ticket, error) {
	ev, err := s.events.Get(ctx, eventID)
	if err != nil {
		if errors.Is(err, event.ErrNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if err := s.seen(&p, ev); err != nil {
		return nil, err
	}
	if !s.authz.Allow(p, eventResource(ev, authz.TypeTicket), authz.Read) {
		return nil, ErrForbidden
	}
	tickets, err := s.tickets.ListByEvent(ctx, eventID)
	if err != nil {
		return nil, err
	}
	return s.withEvents(ctx, tickets), nil
}

func (s *service) ListByEventStatus(ctx context.Context, p authz.Principal, eventID uuid.UUID, statuses []Status) ([]Ticket, error) {
	if len(statuses) == 1 && statuses[0] == StatusActive {
		return s.ListByEvent(ctx, p, eventID)
	}
	store, err := s.statusStore()
	if err != nil {
		return nil, err
	}
	ev, err := s.events.Get(ctx, eventID)
	if err != nil {
		if errors.Is(err, event.ErrNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if err := s.seen(&p, ev); err != nil {
		return nil, err
	}
	if !s.authz.Allow(p, eventResource(ev, authz.TypeTicket), authz.Read) {
		return nil, ErrForbidden
	}
	tickets, err := store.ListByEventStatus(ctx, eventID, statuses)
	if err != nil {
		return nil, err
	}
	return s.withEvents(ctx, tickets), nil
}

func (s *service) Get(ctx context.Context, p authz.Principal, id uuid.UUID) (Ticket, error) {
	t, err := s.tickets.Get(ctx, id)
	if err != nil {
		return Ticket{}, err
	}
	if err := s.readable(ctx, p, t); err != nil {
		return Ticket{}, err
	}
	return s.withEvent(ctx, t), nil
}

func (s *service) GetByUserEvent(ctx context.Context, p authz.Principal, userID, eventID uuid.UUID) (Ticket, error) {
	t, err := s.tickets.GetByOwnerEvent(ctx, userID, eventID)
	if err != nil {
		return Ticket{}, err
	}
	if err := s.readable(ctx, p, t); err != nil {
		return Ticket{}, err
	}
	// "Is this person registered?": only an active Ticket says yes. sky-app
	// shows "Kayıtlısın" on a 200 and hides the join button, so a cancelled
	// (or waitlisted) Ticket answers 404 and the person may register again
	// (docs/ticket-cancellation.md).
	if !t.Active() {
		return Ticket{}, ErrNotFound
	}
	return s.withEvent(ctx, t), nil
}

func (s *service) ListQuery(ctx context.Context, p authz.Principal, email string, userID *uuid.UUID) ([]Ticket, error) {
	email = strings.TrimSpace(email)
	if email == "" && userID == nil {
		return nil, invalid(CodeTicketQueryRequired)
	}

	var byUser []Ticket
	if userID != nil {
		listed, err := s.tickets.ListByOwner(ctx, *userID)
		if err != nil {
			return nil, err
		}
		byUser = listed
	}

	ownEmail := false
	var byEmail []Ticket
	if email != "" {
		if s.users != nil {
			found, err := s.users.FindByEmail(ctx, email)
			if err != nil {
				return nil, err
			}
			for _, u := range found {
				if p.ID == u.ID.String() {
					ownEmail = true
				}
				listed, err := s.tickets.ListByOwner(ctx, u.ID)
				if err != nil {
					return nil, err
				}
				byEmail = append(byEmail, listed...)
			}
		}
		guests, err := s.tickets.ListByGuestEmail(ctx, email)
		if err != nil {
			return nil, err
		}
		byEmail = append(byEmail, guests...)
	}

	var tickets []Ticket
	switch {
	case userID != nil && email != "":
		tickets = intersectTickets(byUser, byEmail)
	case userID != nil:
		tickets = byUser
	default:
		tickets = byEmail
	}

	seen := map[uuid.UUID]struct{}{}
	uniq := make([]Ticket, 0, len(tickets))
	for _, t := range tickets {
		// "Who has a Ticket" means an active one.
		if !t.Active() {
			continue
		}
		if _, ok := seen[t.ID]; ok {
			continue
		}
		seen[t.ID] = struct{}{}
		uniq = append(uniq, t)
	}

	ownUser := userID != nil && p.ID == userID.String()
	own := ownUser
	if userID == nil && ownEmail {
		own = true
	}

	if own {
		return s.withEvents(ctx, uniq), nil
	}
	visible := make([]Ticket, 0, len(uniq))
	for _, t := range uniq {
		if s.canRead(ctx, p, t) {
			visible = append(visible, t)
		}
	}
	if len(visible) == 0 && !s.authz.Allow(p, authz.Resource{Type: authz.TypeTicket}, authz.Read) && !hasLeaderGroup(p) {
		return nil, ErrForbidden
	}
	return s.withEvents(ctx, visible), nil
}

func intersectTickets(a, b []Ticket) []Ticket {
	inB := map[uuid.UUID]struct{}{}
	for _, t := range b {
		inB[t.ID] = struct{}{}
	}
	out := make([]Ticket, 0)
	for _, t := range a {
		if _, ok := inB[t.ID]; ok {
			out = append(out, t)
		}
	}
	return out
}

// hasLeaderGroup reports whether p leads some team, by whole path segment
// (authz.IsAnyLeader). The Privileged answer is the authorizer's: the Ticket
// read check next to every use.
func hasLeaderGroup(p authz.Principal) bool {
	return authz.IsAnyLeader(p, authz.DefaultPolicy().LeaderSubgroups)
}

// CheckIn is the door's check-in of a Ticket it picked (the desk): the
// Check-in, and the day's Venue entry (desk) when the Ticket has none.
func (s *service) CheckIn(ctx context.Context, p authz.Principal, ticketID, sessionID uuid.UUID) (CheckIn, error) {
	return s.checkIn(ctx, p, ticketID, sessionID, false)
}

// checkIn: scan is a credential the door scanned (Ticket QR, SkyPass,
// Wallet, card), which the Event's entry mode decides.
func (s *service) checkIn(ctx context.Context, p authz.Principal, ticketID, sessionID uuid.UUID, scan bool) (CheckIn, error) {
	t, err := s.tickets.Get(ctx, ticketID)
	if err != nil {
		return CheckIn{}, err
	}
	ev, err := s.events.Get(ctx, t.EventID)
	if err != nil {
		if errors.Is(err, event.ErrNotFound) {
			return CheckIn{}, ErrNotFound
		}
		return CheckIn{}, err
	}
	if err := s.openForCheckIn(&p, ev); err != nil {
		return CheckIn{}, err
	}
	if !s.canUseDoor(ctx, p, ev) {
		return CheckIn{}, ErrForbidden
	}
	if scan {
		// A scan at the door: in the venue mode the day's Venue entry
		// alone, else the Check-in and the day's entry if none yet
		// (docs/venue-entry.md). A scan that asks for the Session's
		// Check-in (WithSessionCheckIn) writes it in either mode.
		how := staffEntry(EntryScan, p)
		if venueMode(ev) && !sessionCheckInAsked(ctx) {
			return s.enterAtSession(ctx, t, sessionID, how)
		}
		return s.addSessionCheckIn(ctx, t, sessionID, how)
	}
	return s.addSessionCheckIn(ctx, t, sessionID, staffEntry(EntryDesk, p))
}

// eventOf is the current Event id names.
func (s *service) eventOf(ctx context.Context, id uuid.UUID) (event.Event, error) {
	ev, err := s.events.Get(ctx, id)
	if errors.Is(err, event.ErrNotFound) {
		return event.Event{}, ErrNotFound
	}
	return ev, err
}

// ticketSession is the Session sessionID names, of t's Event: ErrNotFound
// for no such Session (or Day), ErrInvalid for one of another Event.
func (s *service) ticketSession(ctx context.Context, t Ticket, sessionID uuid.UUID) (event.Session, error) {
	sess, _, err := s.ticketSessionDay(ctx, t, sessionID)
	return sess, err
}

// ticketSessionDay is ticketSession with the Session's EventDay.
func (s *service) ticketSessionDay(ctx context.Context, t Ticket, sessionID uuid.UUID) (event.Session, event.Day, error) {
	sess, err := s.events.GetSession(ctx, sessionID)
	if err != nil {
		if errors.Is(err, event.ErrNotFound) {
			return event.Session{}, event.Day{}, ErrNotFound
		}
		return event.Session{}, event.Day{}, err
	}
	day, err := s.events.GetDay(ctx, sess.EventDayID)
	if err != nil {
		if errors.Is(err, event.ErrNotFound) {
			return event.Session{}, event.Day{}, ErrNotFound
		}
		return event.Session{}, event.Day{}, err
	}
	if day.EventID != t.EventID {
		return event.Session{}, event.Day{}, invalid(CodeSessionNotInEvent)
	}
	return sess, day, nil
}

// addSessionCheckIn is the one door every Session Check-in goes through:
// the Check-in, and in the same write the day's Venue entry the way how
// says when the Ticket has none that day (docs/venue-entry.md). Nobody is
// refused for not having been scanned at the door.
func (s *service) addSessionCheckIn(ctx context.Context, t Ticket, sessionID uuid.UUID, how entryBy) (CheckIn, error) {
	sess, day, err := s.ticketSessionDay(ctx, t, sessionID)
	if err != nil {
		return CheckIn{}, err
	}
	// Only an active Ticket opens the door: every way in (scan, SkyPass,
	// me, resolve, guest, code) comes here.
	if refused := inactiveErr(t); refused != nil {
		return CheckIn{}, refused
	}
	dup, err := s.tickets.HasCheckIn(ctx, t.ID, sessionID)
	if err != nil {
		return CheckIn{}, err
	}
	if dup {
		return CheckIn{}, s.duplicate(ctx, ErrAlreadyCheckedIn, t.ID, "", sessionID)
	}
	entry, err := s.venueEntryFor(ctx, t, day, sess, how)
	if err != nil {
		return CheckIn{}, err
	}
	scannedAt, late := s.scanned(ctx)
	ci, err := s.tickets.AddCheckIn(ctx, CheckIn{
		TicketID: t.ID, SessionID: sessionID, EventDayID: sess.EventDayID, Entry: entry,
		ScannedAt: scannedAt, NoMail: late, Source: checkInSource(how.source), ByUserID: how.by,
	})
	if err != nil {
		return CheckIn{}, s.duplicate(ctx, err, t.ID, "", sessionID)
	}
	if scannedAt != nil {
		s.scans.recordOffline(nil)
	}
	if ci.EntryWritten {
		s.scans.recordEntry(how.source)
	}
	ci.Kind = KindCheckIn
	return ci, nil
}

func (s *service) ticketEventID(ctx context.Context, sessionID uuid.UUID) (uuid.UUID, error) {
	_, eventID, err := s.sessionAndEvent(ctx, sessionID)
	return eventID, err
}

// sessionAndEvent reads a Session and the id of the Event its day belongs to.
func (s *service) sessionAndEvent(ctx context.Context, sessionID uuid.UUID) (event.Session, uuid.UUID, error) {
	sess, err := s.events.GetSession(ctx, sessionID)
	if err != nil {
		if errors.Is(err, event.ErrNotFound) {
			return event.Session{}, uuid.Nil, ErrNotFound
		}
		return event.Session{}, uuid.Nil, err
	}
	day, err := s.events.GetDay(ctx, sess.EventDayID)
	if err != nil {
		if errors.Is(err, event.ErrNotFound) {
			return event.Session{}, uuid.Nil, ErrNotFound
		}
		return event.Session{}, uuid.Nil, err
	}
	return sess, day.EventID, nil
}

func (s *service) CheckInMe(ctx context.Context, p authz.Principal, sessionID uuid.UUID, doorToken string) (CheckIn, error) {
	withToken := strings.TrimSpace(doorToken) != ""
	ci, err := s.checkInMe(ctx, p, sessionID, doorToken)
	s.door.RecordSelf(withToken, guestCheckInOutcome(err))
	return ci, err
}

// checkInMe: without a token and in SESSION_QR_MODE=open it is the static
// Session QR's check-in, unchanged. A token, or qr mode, adds what the door
// QR proves: the Session's time window and a fresh code from the screen in
// the room.
func (s *service) checkInMe(ctx context.Context, p authz.Principal, sessionID uuid.UUID, doorToken string) (CheckIn, error) {
	ownerID, err := uuid.Parse(p.ID)
	if err != nil {
		return CheckIn{}, invalid(CodeTokenSubjectInvalid)
	}
	sess, eventID, err := s.sessionAndEvent(ctx, sessionID)
	if err != nil {
		return CheckIn{}, err
	}
	ev, err := s.eventOf(ctx, eventID)
	if err != nil {
		return CheckIn{}, err
	}
	if err := s.openForCheckIn(&p, ev); err != nil {
		return CheckIn{}, err
	}
	withToken := strings.TrimSpace(doorToken) != ""
	if (withToken || s.door.SessionMode() == doorqr.ModeQR) &&
		!s.door.SessionOpen(sess.StartTime, sess.EndTime, sess.Cancelled) {
		return CheckIn{}, ErrSessionClosed
	}
	use, err := s.admitDoorQR(doorToken, sess, sessionID, eventID, s.door.SessionMode())
	if err != nil {
		return CheckIn{}, err
	}
	t, err := s.tickets.GetByOwnerEvent(ctx, ownerID, eventID)
	if err == nil {
		var ci CheckIn
		ci, err = s.addSessionCheckIn(ctx, t, sessionID, selfEntry)
		if err == nil {
			return ci, nil
		}
	}
	// As for guests: only a check-in that happened (or already had) spends
	// a place on the token.
	if !errors.Is(err, ErrConflict) {
		use.Release()
	}
	return CheckIn{}, err
}

func (s *service) CheckInGuest(ctx context.Context, sessionID uuid.UUID, g GuestCheckIn) (CheckIn, error) {
	withToken := strings.TrimSpace(g.DoorToken) != ""
	ci, err := s.checkInGuest(ctx, sessionID, g)
	s.door.Record(withToken, guestCheckInOutcome(err))
	return ci, err
}

func (s *service) checkInGuest(ctx context.Context, sessionID uuid.UUID, g GuestCheckIn) (CheckIn, error) {
	email, key := strings.TrimSpace(g.Email), strings.TrimSpace(g.TicketKey)
	if email == "" && key == "" {
		return CheckIn{}, invalid(CodeEmailOrTicketKeyRequired)
	}
	sess, eventID, err := s.sessionAndEvent(ctx, sessionID)
	if err != nil {
		return CheckIn{}, err
	}
	// A guest is signed out: a draft does not exist for them.
	ev, err := s.eventOf(ctx, eventID)
	if err != nil {
		return CheckIn{}, err
	}
	if err := s.openForCheckIn(nil, ev); err != nil {
		return CheckIn{}, err
	}
	if !s.door.SessionOpen(sess.StartTime, sess.EndTime, sess.Cancelled) {
		return CheckIn{}, ErrSessionClosed
	}
	// The door QR is checked before any Ticket is looked up, so without one
	// (in qr mode) the route says nothing about which e-mails are
	// registered.
	use, err := s.admitDoorQR(g.DoorToken, sess, sessionID, eventID, s.door.Mode())
	if err != nil {
		return CheckIn{}, err
	}
	withToken := strings.TrimSpace(g.DoorToken) != ""
	ci, err := s.checkInGuestTicket(ctx, sessionID, eventID, email, key, withToken)
	// Only a guest who is now checked in (or already was) spends a place on
	// the token; an e-mail without a Ticket gives it back, so junk e-mails
	// cannot use a door QR up. The route's per-address budget limits those.
	if err != nil && !errors.Is(err, ErrConflict) {
		use.Release()
	}
	if errors.Is(err, ErrNotFound) {
		// Whoever has no Ticket is shown where to register, while the
		// Event takes registrations. The token was checked first, so this
		// tells nothing a stranger could not read on the Event's page.
		// Without a token (open mode) a member's Ticket was not looked
		// for: "register" could be wrong, so no link.
		link := ""
		if withToken {
			link = registrationLink(ev)
		}
		return ci, &GuestNotFoundError{RegistrationURL: link}
	}
	return ci, err
}

// checkInGuestTicket finds the Ticket of a self check-in on the page: by
// its access key (any Ticket of the Event, a member's too), or by e-mail: a
// guest Ticket written for it first, then the Ticket of the member whose
// account e-mail (primary or school) it is.
//
// The key and the member's e-mail need a valid door or attendance token in
// every mode (bilet-qr-ve-pdf 19): GUEST_SELF_CHECKIN_MODE=open keeps only
// the guest's e-mail without one, as before. withToken says the token was
// sent (admitDoorQR has checked it by now).
func (s *service) checkInGuestTicket(ctx context.Context, sessionID, eventID uuid.UUID, email, key string, withToken bool) (CheckIn, error) {
	if key != "" {
		if !withToken {
			return CheckIn{}, ErrDoorQRRequired
		}
		t, err := s.tickets.GetByAccessKey(ctx, key)
		if err != nil {
			return CheckIn{}, err
		}
		// Another Event's key finds nothing here, as an unknown one.
		if t.EventID != eventID {
			return CheckIn{}, ErrNotFound
		}
		return s.addSessionCheckIn(ctx, t, sessionID, selfEntry)
	}
	listed, err := s.tickets.ListByGuestEmail(ctx, email)
	if err != nil {
		return CheckIn{}, err
	}
	for _, t := range listed {
		if t.EventID == eventID {
			return s.addSessionCheckIn(ctx, t, sessionID, selfEntry)
		}
	}
	if !withToken {
		// open mode without a token: the guest's e-mail alone, as before.
		return CheckIn{}, ErrNotFound
	}
	return s.checkInMemberByEmail(ctx, sessionID, eventID, email)
}

// schoolEmailFinder finds active accounts by their school e-mail
// (user.PostgresStore, user.MemoryStore).
type schoolEmailFinder interface {
	FindBySchoolEmail(ctx context.Context, email string) ([]user.User, error)
}

// checkInMemberByEmail checks in the member whose account e-mail is email
// (primary, or school when the store reads it) and who holds a Ticket of
// the Event; ErrNotFound for anyone else.
func (s *service) checkInMemberByEmail(ctx context.Context, sessionID, eventID uuid.UUID, email string) (CheckIn, error) {
	if s.users == nil {
		return CheckIn{}, ErrNotFound
	}
	holders, err := s.users.FindByEmail(ctx, email)
	if err != nil {
		return CheckIn{}, err
	}
	if finder, ok := s.users.(schoolEmailFinder); ok {
		school, err := finder.FindBySchoolEmail(ctx, email)
		if err != nil {
			return CheckIn{}, err
		}
		holders = append(holders, school...)
	}
	seen := map[uuid.UUID]bool{}
	for _, u := range holders {
		if seen[u.ID] {
			continue
		}
		seen[u.ID] = true
		t, err := s.tickets.GetByOwnerEvent(ctx, u.ID, eventID)
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return CheckIn{}, err
		}
		return s.addSessionCheckIn(ctx, t, sessionID, selfEntry)
	}
	return CheckIn{}, ErrNotFound
}

// registrationLink is where to register for ev while it takes
// registrations: its form, else its external registration link; "" once
// registration has closed.
func registrationLink(ev event.Event) string {
	if !ev.RegistrationOpen {
		return ""
	}
	if ev.FormURL != "" {
		return ev.FormURL
	}
	return ev.ExternalRegistrationURL
}

// admitDoorQR applies a self check-in mode (GUEST_SELF_CHECKIN_MODE for
// guests, SESSION_QR_MODE for check-in/me): qr requires a valid token; open
// takes none, but a token that is sent must be valid either way. Door and
// display tokens are both accepted, each with its own use cap; a static
// token too, for a Session with a start and an end (the Session window,
// checked before, is all that bounds it).
func (s *service) admitDoorQR(token string, sess event.Session, sessionID, eventID uuid.UUID, mode doorqr.Mode) (doorqr.Use, error) {
	if strings.TrimSpace(token) == "" {
		if mode == doorqr.ModeQR {
			return doorqr.Use{}, ErrDoorQRRequired
		}
		return doorqr.Use{}, nil
	}
	use, err := s.door.Admit(token, sessionID, eventID)
	switch {
	case err == nil && use.Static() && (sess.StartTime == nil || sess.EndTime == nil):
		// Without both times the window would leave a side open, and a
		// static QR never expires on its own.
		return doorqr.Use{}, ErrDoorQRInvalid
	case err == nil:
		return use, nil
	case errors.Is(err, doorqr.ErrExpired):
		return doorqr.Use{}, ErrDoorQRExpired
	case errors.Is(err, doorqr.ErrUsedUp):
		return doorqr.Use{}, ErrDoorQRUsedUp
	default:
		return doorqr.Use{}, ErrDoorQRInvalid
	}
}

func guestCheckInOutcome(err error) doorqr.Outcome {
	switch {
	case err == nil:
		return doorqr.OutcomeCheckedIn
	case errors.Is(err, ErrConflict):
		return doorqr.OutcomeAlready
	case errors.Is(err, ErrNotFound):
		return doorqr.OutcomeNotFound
	case errors.Is(err, ErrInvalid):
		return doorqr.OutcomeBadRequest
	case errors.Is(err, ErrSessionClosed):
		return doorqr.OutcomeSessionClosed
	case errors.Is(err, ErrDoorQRRequired):
		return doorqr.OutcomeRequired
	case errors.Is(err, ErrDoorQRInvalid):
		return doorqr.OutcomeInvalid
	case errors.Is(err, ErrDoorQRExpired):
		return doorqr.OutcomeExpired
	case errors.Is(err, ErrDoorQRUsedUp):
		return doorqr.OutcomeUsedUp
	default:
		return doorqr.OutcomeFailed
	}
}

// MintDoorQR signs a door QR, or an attendance QR for a projection screen,
// for a Session. Whoever may take check-ins at the Event's door may show one
// (canUseDoor: door staff, the owning team's leaders, Privileged, team door
// scan).
func (s *service) MintDoorQR(ctx context.Context, p authz.Principal, sessionID uuid.UUID, class doorqr.Class) (DoorQR, error) {
	ev, err := s.authorizedDoorEvent(ctx, p, sessionID, true)
	if err != nil {
		return DoorQR{}, err
	}
	sess, err := s.events.GetSession(ctx, sessionID)
	if err != nil {
		if errors.Is(err, event.ErrNotFound) {
			return DoorQR{}, ErrNotFound
		}
		return DoorQR{}, err
	}
	if class == doorqr.ClassStatic {
		return s.mintStaticQR(sess, ev.ID)
	}
	// A QR no guest could use would only confuse the door.
	if !s.door.SessionOpen(sess.StartTime, sess.EndTime, sess.Cancelled) {
		return DoorQR{}, ErrSessionClosed
	}
	pass, err := s.door.MintFor(class, sessionID, ev.ID)
	if err != nil {
		return DoorQR{}, err
	}
	return DoorQR{
		Token:               pass.Token,
		URL:                 pass.URL,
		Use:                 string(pass.Class),
		MaxUses:             pass.MaxUses,
		SessionQRMode:       string(s.door.SessionMode()),
		SessionID:           sessionID,
		EventID:             ev.ID,
		IssuedAt:            pass.IssuedAt,
		ExpiresAt:           pass.ExpiresAt,
		RefreshAfterSeconds: int(pass.RefreshAfter / time.Second),
	}, nil
}

// mintStaticQR answers the static attendance QR: the same token for the
// Session every time, so it can be printed or shown ahead. It is minted
// outside the Session window too; validFrom and validUntil say when it checks
// anyone in (the rotating QR's window, DOOR_QR_SESSION_GRACE).
func (s *service) mintStaticQR(sess event.Session, eventID uuid.UUID) (DoorQR, error) {
	if !s.door.StaticOn() {
		return DoorQR{}, invalid(CodeDoorQRStaticOff)
	}
	if sess.Cancelled {
		return DoorQR{}, ErrSessionClosed
	}
	if sess.StartTime == nil || sess.EndTime == nil {
		return DoorQR{}, invalid(CodeSessionTimesRequired)
	}
	pass, err := s.door.MintFor(doorqr.ClassStatic, sess.ID, eventID)
	if err != nil {
		return DoorQR{}, err
	}
	from, until := s.door.SessionWindow(sess.StartTime, sess.EndTime)
	return DoorQR{
		Token:         pass.Token,
		URL:           pass.URL,
		Use:           string(pass.Class),
		SessionQRMode: string(s.door.SessionMode()),
		SessionID:     sess.ID,
		EventID:       eventID,
		IssuedAt:      pass.IssuedAt,
		ExpiresAt:     *until,
		ValidFrom:     from,
		ValidUntil:    until,
	}, nil
}

func (s *service) CheckInUser(ctx context.Context, p authz.Principal, sessionID, userID uuid.UUID) (CheckIn, error) {
	eventID, err := s.ticketEventID(ctx, sessionID)
	if err != nil {
		return CheckIn{}, err
	}
	t, err := s.tickets.GetByOwnerEvent(ctx, userID, eventID)
	if err != nil {
		return CheckIn{}, err
	}
	return s.checkIn(ctx, p, t.ID, sessionID, true)
}

func normalizedDoorValue(value string) string {
	return strings.Join(strings.Fields(strings.ToLower(strings.TrimSpace(value))), " ")
}

func summaryDoorIdentity(summary PersonSummary) (string, []string) {
	name := strings.TrimSpace(strings.Join([]string{summary.FirstName, summary.LastName}, " "))
	if name == "" {
		name = strings.TrimSpace(summary.Email)
	}
	return name, []string{summary.Email, name, summary.ID.String()}
}

func (s *service) localDoorTicketIdentity(ctx context.Context, t Ticket) (string, []string, bool) {
	if t.TicketType == Guest {
		name := strings.TrimSpace(strings.Join([]string{t.GuestFirstName, t.GuestLastName}, " "))
		if name == "" {
			name = strings.TrimSpace(t.GuestEmail)
		}
		return name, []string{t.GuestEmail, name}, true
	}
	if t.OwnerID == nil {
		return "", nil, false
	}
	if s.users != nil {
		if shadow, err := s.users.Get(ctx, *t.OwnerID); err == nil {
			name, values := summaryDoorIdentity(PersonSummary{
				ID: shadow.ID, Email: shadow.Email, FirstName: shadow.FirstName, LastName: shadow.LastName,
			})
			return name, values, true
		}
	}
	if t.Owner != nil {
		name, values := summaryDoorIdentity(*t.Owner)
		return name, values, true
	}
	return t.OwnerID.String(), []string{t.OwnerID.String()}, false
}

func (s *service) doorTicketIdentity(ctx context.Context, t Ticket) (string, []string) {
	name, values, found := s.localDoorTicketIdentity(ctx, t)
	if found || t.OwnerID == nil || s.people == nil {
		return name, values
	}
	person, err := s.people.GetUser(ctx, *t.OwnerID)
	if err != nil {
		return name, values
	}
	return summaryDoorIdentity(PersonSummary{
		ID: person.ID, Email: person.Email, FirstName: person.FirstName, LastName: person.LastName,
	})
}

// authorizedDoorEvent is the Event of the Session whose door p is about to
// use: to take a check-in (checkIn, which a draft refuses: openForCheckIn)
// or to read it (a draft only for who may see it).
func (s *service) authorizedDoorEvent(
	ctx context.Context,
	p authz.Principal,
	sessionID uuid.UUID,
	checkIn bool,
) (event.Event, error) {
	eventID, err := s.ticketEventID(ctx, sessionID)
	if err != nil {
		return event.Event{}, err
	}
	ev, err := s.eventOf(ctx, eventID)
	if err != nil {
		return event.Event{}, err
	}
	gate := s.seen
	if checkIn {
		gate = s.openForCheckIn
	}
	if err := gate(&p, ev); err != nil {
		return event.Event{}, err
	}
	if !s.canUseDoor(ctx, p, ev) {
		return event.Event{}, ErrForbidden
	}
	return ev, nil
}

func (s *service) AuthorizeSessionDoor(ctx context.Context, p authz.Principal, sessionID uuid.UUID) error {
	_, err := s.authorizedDoorEvent(ctx, p, sessionID, true)
	return err
}

func (s *service) ResolveAndCheckIn(
	ctx context.Context,
	p authz.Principal,
	sessionID uuid.UUID,
	target DoorCheckInTarget,
) (DoorCheckIn, error) {
	ev, err := s.authorizedDoorEvent(ctx, p, sessionID, true)
	if err != nil {
		return DoorCheckIn{}, err
	}
	t, name, err := s.findDoorTicket(ctx, ev, target)
	if err != nil {
		return DoorCheckIn{}, err
	}
	ci, err := s.addSessionCheckIn(ctx, t, sessionID, staffEntry(EntryDesk, p))
	if err != nil {
		return DoorCheckIn{}, err
	}
	return DoorCheckIn{
		ID: ci.ID, TicketID: ci.TicketID, SessionID: ci.SessionID, EventDayID: ci.EventDayID,
		PersonName: name, CreatedAt: ci.CreatedAt, Kind: KindCheckIn,
	}, nil
}

// findDoorTicket is the one Ticket of ev the desk's target names, with the
// name the door shows: ErrInvalid for no target, ErrNotFound for none,
// ErrAmbiguous for more than one. A Ticket's code first, then the person,
// the name and e-mail (docs/ticket-qr.md).
func (s *service) findDoorTicket(ctx context.Context, ev event.Event, target DoorCheckInTarget) (Ticket, string, error) {
	query := normalizedDoorValue(target.Query)
	if target.PersonID == nil && query == "" {
		return Ticket{}, "", invalid(CodeDoorQueryRequired)
	}
	if target.PersonID == nil {
		row, found, err := s.doorTicketByCode(ctx, ev.ID, target.Query)
		if err != nil {
			return Ticket{}, "", err
		}
		if found {
			return row.Ticket, row.Name, nil
		}
	}
	if store, ok := s.tickets.(DoorStore); ok {
		return s.findStoredDoorTicket(ctx, store, ev, target, query)
	}
	listed, err := s.everyTicket(ctx, ev.ID)
	if err != nil {
		return Ticket{}, "", err
	}
	type match struct {
		ticket Ticket
		name   string
	}
	matches := make([]match, 0, 1)
	for _, candidate := range listed {
		if target.PersonID != nil {
			if candidate.OwnerID != nil && *candidate.OwnerID == *target.PersonID {
				name, _ := s.doorTicketIdentity(ctx, candidate)
				matches = append(matches, match{ticket: candidate, name: name})
			}
			continue
		}
		name, values, found := s.localDoorTicketIdentity(ctx, candidate)
		if !found {
			continue
		}
		for _, value := range values {
			if normalizedDoorValue(value) == query {
				matches = append(matches, match{ticket: candidate, name: name})
				break
			}
		}
	}
	if len(matches) == 0 && target.PersonID == nil && query != "" && s.directory != nil {
		people, _ := s.directory.SearchUsers(ctx, target.Query, assignableUserLimit)
		directoryMatches := make(map[uuid.UUID]PersonSummary)
		for _, person := range people {
			summary, _ := s.personSummary(ctx, person, true)
			_, values := summaryDoorIdentity(summary)
			for _, value := range values {
				if normalizedDoorValue(value) == query {
					directoryMatches[summary.ID] = summary
					break
				}
			}
		}
		for _, candidate := range listed {
			if candidate.OwnerID == nil {
				continue
			}
			if summary, ok := directoryMatches[*candidate.OwnerID]; ok {
				name, _ := summaryDoorIdentity(summary)
				matches = append(matches, match{ticket: candidate, name: name})
			}
		}
	}
	if len(matches) == 0 {
		return Ticket{}, "", ErrNotFound
	}
	if len(matches) > 1 {
		return Ticket{}, "", ErrAmbiguous
	}
	return matches[0].ticket, matches[0].name, nil
}

func (s *service) findStoredDoorTicket(
	ctx context.Context,
	store DoorStore,
	ev event.Event,
	target DoorCheckInTarget,
	query string,
) (Ticket, string, error) {
	var rows []DoorTicketIdentity
	var err error
	if target.PersonID != nil {
		rows, err = store.DoorTicketsByOwners(ctx, ev.ID, []uuid.UUID{*target.PersonID})
	} else {
		rows, err = store.SearchDoorTickets(ctx, ev.ID, query, true, 2)
	}
	if err != nil {
		return Ticket{}, "", err
	}
	if len(rows) == 0 && target.PersonID == nil && s.directory != nil {
		people, _ := s.directory.SearchUsers(ctx, target.Query, assignableUserLimit)
		summaries := make(map[uuid.UUID]PersonSummary)
		ownerIDs := make([]uuid.UUID, 0, len(people))
		for _, person := range people {
			summary := PersonSummary{
				ID: person.ID, Email: person.Email, FirstName: person.FirstName, LastName: person.LastName,
			}
			_, values := summaryDoorIdentity(summary)
			for _, value := range values {
				if normalizedDoorValue(value) == query {
					summaries[summary.ID] = summary
					ownerIDs = append(ownerIDs, summary.ID)
					break
				}
			}
		}
		rows, err = store.DoorTicketsByOwners(ctx, ev.ID, ownerIDs)
		if err != nil {
			return Ticket{}, "", err
		}
		for i := range rows {
			if rows[i].Ticket.OwnerID == nil {
				continue
			}
			summary, found := summaries[*rows[i].Ticket.OwnerID]
			if !found {
				continue
			}
			if rows[i].Found {
				values := []string{rows[i].Email, rows[i].Name, rows[i].Ticket.OwnerID.String()}
				matched := false
				for _, value := range values {
					if normalizedDoorValue(value) == query {
						matched = true
						break
					}
				}
				if !matched {
					rows[i].Ticket.ID = uuid.Nil
				}
			} else {
				rows[i].Name, _ = summaryDoorIdentity(summary)
			}
		}
		rows = slices.DeleteFunc(rows, func(row DoorTicketIdentity) bool {
			return row.Ticket.ID == uuid.Nil
		})
	}
	if len(rows) == 0 {
		return Ticket{}, "", ErrNotFound
	}
	if len(rows) > 1 {
		return Ticket{}, "", ErrAmbiguous
	}
	return rows[0].Ticket, rows[0].Name, nil
}

func (s *service) DoorActivity(
	ctx context.Context,
	p authz.Principal,
	sessionID uuid.UUID,
) (DoorActivity, error) {
	ev, err := s.authorizedDoorEvent(ctx, p, sessionID, false)
	if err != nil {
		return DoorActivity{}, err
	}
	activity, err := s.sessionActivity(ctx, ev, sessionID)
	if err != nil || !venueMode(ev) {
		return activity, err
	}
	return s.withDayEntries(ctx, ev, sessionID, activity)
}

// SessionCheckIns is the Session's door activity without the day's Venue
// entries the venue mode adds: its Check-ins alone (kind check_in), and
// how many it has. The panel's door counts a Session by it.
func (s *service) SessionCheckIns(
	ctx context.Context,
	p authz.Principal,
	sessionID uuid.UUID,
) (DoorActivity, error) {
	ev, err := s.authorizedDoorEvent(ctx, p, sessionID, false)
	if err != nil {
		return DoorActivity{}, err
	}
	return s.sessionActivity(ctx, ev, sessionID)
}

// doorActivityLimit is how many of the latest the door activity lists.
const doorActivityLimit = 20

// withDayEntries adds the day's Venue entries to a Session's door activity
// in the venue mode (kind entry), where the door's scans write them: the
// door shows who came in today (docs/venue-entry.md). An entry is listed
// with the Session asked for, so the door's undo by id finds it.
func (s *service) withDayEntries(ctx context.Context, ev event.Event, sessionID uuid.UUID, activity DoorActivity) (DoorActivity, error) {
	sess, day, err := s.ticketSessionDay(ctx, Ticket{EventID: ev.ID}, sessionID)
	if err != nil {
		return DoorActivity{}, err
	}
	date, err := s.sessionEntryDate(ctx, ev.ID, day, sess)
	if err != nil {
		return DoorActivity{}, err
	}
	entries, err := s.namedEntries(ctx, ev.ID, date)
	if err != nil {
		return DoorActivity{}, err
	}
	for _, e := range entries {
		activity.Items = append(activity.Items, DoorCheckIn{
			ID: e.ID, TicketID: e.TicketID, SessionID: sessionID, EventDayID: day.ID,
			PersonName: e.Name, CreatedAt: e.CreatedAt, Kind: KindEntry,
		})
	}
	activity.Total += len(entries)
	sort.SliceStable(activity.Items, func(i, j int) bool {
		return activity.Items[i].CreatedAt.After(activity.Items[j].CreatedAt)
	})
	if len(activity.Items) > doorActivityLimit {
		activity.Items = activity.Items[:doorActivityLimit]
	}
	return activity, nil
}

// namedEntries are the Event's Venue entries on date, each with the name
// the door shows; a name the store did not know is the Ticket's.
func (s *service) namedEntries(ctx context.Context, eventID uuid.UUID, date string) ([]DoorEntry, error) {
	entries, err := s.tickets.VenueEntriesOn(ctx, eventID, date)
	if err != nil {
		return nil, err
	}
	for i := range entries {
		if entries[i].Name != "" {
			continue
		}
		if t, err := s.tickets.Get(ctx, entries[i].TicketID); err == nil {
			entries[i].Name, _, _ = s.localDoorTicketIdentity(ctx, t)
		}
	}
	return entries, nil
}

// sessionActivity is the Session's latest check-ins (kind check_in) and
// how many it has.
func (s *service) sessionActivity(ctx context.Context, ev event.Event, sessionID uuid.UUID) (DoorActivity, error) {
	if store, ok := s.tickets.(DoorStore); ok {
		return store.DoorSessionActivity(ctx, sessionID, doorActivityLimit)
	}
	listed, err := s.tickets.ListByEvent(ctx, ev.ID)
	if err != nil {
		return DoorActivity{}, err
	}
	type activityItem struct {
		ticket  Ticket
		checkIn CheckIn
	}
	pending := make([]activityItem, 0)
	for _, t := range listed {
		for _, ci := range t.CheckIns {
			if ci.SessionID != sessionID {
				continue
			}
			pending = append(pending, activityItem{ticket: t, checkIn: ci})
		}
	}
	sort.Slice(pending, func(i, j int) bool {
		return pending[i].checkIn.CreatedAt.After(pending[j].checkIn.CreatedAt)
	})
	total := len(pending)
	if len(pending) > doorActivityLimit {
		pending = pending[:doorActivityLimit]
	}
	items := make([]DoorCheckIn, 0, len(pending))
	for _, item := range pending {
		name, _, _ := s.localDoorTicketIdentity(ctx, item.ticket)
		ci := item.checkIn
		items = append(items, DoorCheckIn{
			ID: ci.ID, TicketID: ci.TicketID, SessionID: ci.SessionID, EventDayID: ci.EventDayID,
			PersonName: name, CreatedAt: ci.CreatedAt, Kind: KindCheckIn,
		})
	}
	return DoorActivity{Total: total, Items: items}, nil
}

func doorResource(ev event.Event) authz.Resource {
	return eventResource(ev, authz.TypeTicket)
}

// doorScanTeams are the Event's organizer teams whose membership counts at
// the door: its Owner team and its "members" Co-organizer teams
// (docs/co-organizers.md), those p is in alone (memberOfTeam).
func doorScanTeams(p authz.Principal, ev event.Event) []string {
	var out []string
	if memberOfTeam(p, ev.OwnerTeam) {
		out = append(out, ev.OwnerTeam)
	}
	for _, team := range ev.CoOrganizers {
		if team.Management == event.ManagementMembers && memberOfTeam(p, team.Code) {
			out = append(out, team.Code)
		}
	}
	return out
}

// memberOfTeam reports whether one of p's Groups is the team's or below it,
// by its code as a whole path segment.
func memberOfTeam(p authz.Principal, team string) bool {
	if team == "" {
		return false
	}
	marker := "/" + team
	for _, group := range p.Groups {
		if strings.HasSuffix(group, marker) || strings.Contains(group, marker+"/") {
			return true
		}
	}
	return false
}

// canUseDoor reports whether p may work ev's door: door staff, an
// organizer team's Leader or a Co-organizer person, Privileged; or a member
// of an organizer team whose membership counts while that team's Team door
// scan is on, which is read only for the teams p is in.
func (s *service) canUseDoor(ctx context.Context, p authz.Principal, ev event.Event) bool {
	resource := doorResource(ev)
	if s.authz.Allow(p, resource, authz.Validate) {
		return true
	}
	teams := doorScanTeams(p, ev)
	if len(teams) == 0 {
		return false
	}
	for _, team := range teams {
		if s.teamDoorScan(ctx, team) {
			resource.TeamDoorScanTeams = append(resource.TeamDoorScanTeams, team)
		}
	}
	if len(resource.TeamDoorScanTeams) == 0 {
		return false
	}
	return s.authz.Allow(p, resource, authz.Validate)
}

func (s *service) teamDoorScan(ctx context.Context, ownerTeam string) bool {
	if s.teams == nil || ownerTeam == "" {
		return false
	}
	g, err := s.teams.GetGroup(ctx, ownerTeam)
	if err != nil || g.Attributes == nil {
		return false
	}
	return strings.EqualFold(g.Attributes["team_door_scan"], "true")
}

// everyTicket is the Event's Tickets of every status, for the door to say
// why a cancelled one does not open it; the active ones without a
// StatusStore.
func (s *service) everyTicket(ctx context.Context, eventID uuid.UUID) ([]Ticket, error) {
	if store, ok := s.tickets.(StatusStore); ok {
		return store.ListByEventStatus(ctx, eventID, nil)
	}
	return s.tickets.ListByEvent(ctx, eventID)
}
