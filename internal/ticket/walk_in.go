package ticket

import (
	"context"
	"errors"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/teamaudit"
)

// Walk-in at the door ("Kapıda kayıt", docs/walk-in.md): who may take
// check-ins at an Event's door registers a person who has no Ticket. Core
// writes their guest Ticket, its mail goes as any new Ticket's, and in the
// venue Entry mode their entry for the day is written in the same request.
// A person whose Ticket is already there gets no second one: the door is
// told so and the entry is written with that Ticket.

// WalkIn is what the door gives: the name and the e-mail, and optionally a
// phone number, a university, a department and a grade (Grades), for the
// Event's statistics.
type WalkIn struct {
	FirstName   string
	LastName    string
	Email       string
	PhoneNumber string
	University  string
	Department  string
	Grade       string
}

// WalkInResult is what a Walk-in did: created a Ticket (a cancelled one of
// the holder's own reopened counts as written anew), or found the
// person's Ticket already there.
type WalkInResult string

const (
	WalkInCreated  WalkInResult = "created"
	WalkInExisting WalkInResult = "existing"
)

// WalkInOutcome is the door's answer: who, by the Ticket's code; whether the
// Ticket's mail was queued with this write; and the day's entry in the venue
// Entry mode (nil in per_session). Never the e-mail, the phone number or
// the rest of what the guest gave.
type WalkInOutcome struct {
	Result     WalkInResult `json:"result"`
	Name       string       `json:"name"`
	TicketCode string       `json:"ticketCode"`
	TicketMail bool         `json:"ticketMail"`
	Entry      *DoorEntry   `json:"entry"`
}

// Grades are the grade keys a Walk-in takes (the panel shows them in
// Turkish): preparatory year, years 1 to 4, master's, doctorate, graduate,
// other.
var Grades = []string{"prep", "1", "2", "3", "4", "masters", "phd", "graduate", "other"}

// The limits of a Walk-in's fields, in characters.
const (
	MaxWalkInName = 100
	MaxWalkInText = 200
)

// The codes of a Walk-in's refusals (docs/problem-codes.md).
const (
	CodeEmailInvalid        = "email_invalid"
	CodeGuestFieldInvalid   = "guest_field_invalid"
	CodePhoneInvalid        = "phone_invalid"
	CodeGradeInvalid        = "grade_invalid"
	CodeGuestNameTooLong    = "guest_name_too_long"
	CodeUniversityTooLong   = "university_too_long"
	CodeDepartmentTooLong   = "department_too_long"
	walkInAuditEventFull    = "event_full"
	walkInAuditNotFound     = "not_found"
	walkInAuditFailed       = "failed"
	walkInAuditInvalidState = "ticket_inactive"
	walkInAuditExisting     = "existing"
)

// normalizeWalkIn trims w, lowers its e-mail, puts its phone number in one
// form and checks every field: the name and the e-mail are required.
func normalizeWalkIn(w WalkIn) (WalkIn, error) {
	w.FirstName = strings.TrimSpace(w.FirstName)
	w.LastName = strings.TrimSpace(w.LastName)
	w.Email = strings.ToLower(strings.TrimSpace(w.Email))
	w.PhoneNumber = strings.TrimSpace(w.PhoneNumber)
	w.University = strings.TrimSpace(w.University)
	w.Department = strings.TrimSpace(w.Department)
	w.Grade = strings.TrimSpace(w.Grade)
	if missing := missingGuestFields(GuestInfo{FirstName: w.FirstName, LastName: w.LastName, Email: w.Email}); len(missing) > 0 {
		return WalkIn{}, invalidWith(CodeGuestFieldsRequired, map[string]any{"fields": missing})
	}
	if utf8.RuneCountInString(w.FirstName) > MaxWalkInName || utf8.RuneCountInString(w.LastName) > MaxWalkInName {
		return WalkIn{}, invalidWith(CodeGuestNameTooLong, map[string]any{"max": MaxWalkInName})
	}
	// A control character (a tab, a line break, a NUL) in a name or a
	// free text field is refused: it would break the mail, the PDF and the
	// door's list.
	controlled := []string{}
	for _, f := range []struct{ name, value string }{
		{"firstName", w.FirstName}, {"lastName", w.LastName}, {"university", w.University}, {"department", w.Department},
	} {
		if strings.ContainsFunc(f.value, unicode.IsControl) {
			controlled = append(controlled, f.name)
		}
	}
	if len(controlled) > 0 {
		return WalkIn{}, invalidWith(CodeGuestFieldInvalid, map[string]any{"fields": controlled})
	}
	if !plainEmail(w.Email) {
		return WalkIn{}, invalid(CodeEmailInvalid)
	}
	if w.PhoneNumber != "" {
		phone, ok := normalizePhone(w.PhoneNumber)
		if !ok {
			return WalkIn{}, invalid(CodePhoneInvalid)
		}
		w.PhoneNumber = phone
	}
	if utf8.RuneCountInString(w.University) > MaxWalkInText {
		return WalkIn{}, invalidWith(CodeUniversityTooLong, map[string]any{"max": MaxWalkInText})
	}
	if utf8.RuneCountInString(w.Department) > MaxWalkInText {
		return WalkIn{}, invalidWith(CodeDepartmentTooLong, map[string]any{"max": MaxWalkInText})
	}
	if w.Grade != "" && !slices.Contains(Grades, w.Grade) {
		return WalkIn{}, invalid(CodeGradeInvalid)
	}
	return w, nil
}

// plainEmail reports whether s looks like a mailbox address: one @, a local
// part, a domain with a dot, no spaces or quotes. Whether it receives mail
// is not asked: the Ticket's mail tells.
func plainEmail(s string) bool {
	if len(s) > 254 || strings.ContainsFunc(s, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsControl(r) || strings.ContainsRune(`"'<>(),;:\[]`, r)
	}) {
		return false
	}
	local, domain, ok := strings.Cut(s, "@")
	if !ok || local == "" || len(local) > 64 || strings.Contains(domain, "@") {
		return false
	}
	labels := strings.Split(domain, ".")
	if len(labels) < 2 {
		return false
	}
	for _, label := range labels {
		if label == "" || strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") {
			return false
		}
	}
	return true
}

// normalizePhone puts a phone number in one form: a Turkish number, given
// as +90, 90 or 0 and its ten digits, or the ten digits alone, becomes
// +90 and the ten digits; another country's, written with + (or 00) and 8
// to 15 digits, + and its digits. Spaces, dashes, dots and parentheses
// are dropped. Anything else is not a phone number.
func normalizePhone(raw string) (string, bool) {
	digits := strings.Map(func(r rune) rune {
		switch r {
		case ' ', '-', '.', '(', ')', ' ':
			return -1
		}
		return r
	}, raw)
	international := false
	switch {
	case strings.HasPrefix(digits, "+"):
		digits, international = digits[1:], true
	case strings.HasPrefix(digits, "00"):
		digits, international = digits[2:], true
	}
	if digits == "" || strings.ContainsFunc(digits, func(r rune) bool { return r < '0' || r > '9' }) {
		return "", false
	}
	turkish := func(national string) (string, bool) {
		if len(national) != 10 || national[0] == '0' {
			return "", false
		}
		return "+90" + national, true
	}
	if international {
		if strings.HasPrefix(digits, "90") {
			return turkish(digits[2:])
		}
		if len(digits) < 8 || len(digits) > 15 || digits[0] == '0' {
			return "", false
		}
		return "+" + digits, true
	}
	switch {
	case len(digits) == 11 && digits[0] == '0':
		return turkish(digits[1:])
	case len(digits) == 12 && strings.HasPrefix(digits, "90"):
		return turkish(digits[2:])
	case len(digits) == 10:
		return turkish(digits)
	}
	return "", false
}

// WalkIn registers w at the Event's door (docs/walk-in.md). The door's rule
// comes first, as for every door write (eventDoor): ErrNotFound, an
// *EventClosedError (a draft, cancelled, archived or ended Event; a Walk-in
// is also refused once the Event has ended, event_ended), ErrForbidden for
// who may not take check-ins there. Then the fields (normalizeWalkIn).
//
// The person's Ticket is looked for by the e-mail: a guest Ticket written
// for it, then the Ticket of the member whose account e-mail (primary or
// school) it is. An active one is answered existing, nothing written to
// it; a waitlisted one is ErrTicketWaitlisted; a cancelled one is
// ErrTicketCancelled, unless its holder cancelled it: then it is reopened,
// under the capacity, as their registering again would.
//
// Without one, a guest Ticket is written under the capacity: a full Event
// is ErrEventFull, never its waiting list. The write is a Team audit row
// (ticket.walk_in); a row that cannot be opened stops it
// (teamaudit.ErrUnavailable) before anything is written. The Ticket's mail
// goes as any new Ticket's.
//
// In the venue Entry mode the day's entry is written with the Ticket
// (source walk_in for a Ticket the Walk-in wrote, desk for one found); a
// Ticket already in today is an *EntryConflictError naming them.
func (s *service) WalkIn(ctx context.Context, p authz.Principal, eventID uuid.UUID, w WalkIn) (WalkInOutcome, error) {
	ev, err := s.eventDoor(ctx, p, eventID, true)
	if err != nil {
		return WalkInOutcome{}, err
	}
	if ev.EndedAt(s.now()) {
		return WalkInOutcome{}, &EventClosedError{Reason: "event_ended"}
	}
	w, err = normalizeWalkIn(w)
	if err != nil {
		return WalkInOutcome{}, err
	}
	found, ok, err := s.walkInTicket(ctx, ev.ID, w.Email)
	if err != nil {
		return WalkInOutcome{}, err
	}
	if ok {
		switch {
		case found.StatusOf() == StatusWaitlisted:
			return WalkInOutcome{}, ErrTicketWaitlisted
		case found.StatusOf() == StatusCancelled && found.CancelledBy != CancelledByHolder:
			return WalkInOutcome{}, ErrTicketCancelled
		case found.Active():
			return s.walkInEntry(ctx, p, ev, found, WalkInExisting, false)
		}
	}
	record, err := s.beginWalkInAudit(ctx, p, ev)
	if err != nil {
		return WalkInOutcome{}, err
	}
	written, result, err := s.writeWalkIn(ctx, ev, w, found, ok)
	s.finishWalkInAudit(ctx, record, written.ID, result, err)
	if err != nil {
		return WalkInOutcome{}, err
	}
	if result == WalkInExisting {
		// Written by another request since the read (a double submit).
		return s.walkInEntry(ctx, p, ev, written, WalkInExisting, false)
	}
	s.logger.Printf(`{"event":"walk_in","actorId":%q,"eventId":%q,"ticketId":%q}`, p.ID, ev.ID, written.ID)
	return s.walkInEntry(ctx, p, ev, written, WalkInCreated, written.Active() && s.createdMailOn())
}

// walkInTicket is the Ticket of the Event the e-mail holds: the guest
// Ticket written for it, else the Ticket of the member whose account e-mail
// (primary, or school when the store reads it) it is.
func (s *service) walkInTicket(ctx context.Context, eventID uuid.UUID, email string) (Ticket, bool, error) {
	t, err := s.tickets.GetByGuestEvent(ctx, email, eventID)
	if err == nil {
		return t, true, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return Ticket{}, false, err
	}
	if s.users == nil {
		return Ticket{}, false, nil
	}
	holders, err := s.users.FindByEmail(ctx, email)
	if err != nil {
		return Ticket{}, false, err
	}
	if finder, ok := s.users.(schoolEmailFinder); ok {
		school, err := finder.FindBySchoolEmail(ctx, email)
		if err != nil {
			return Ticket{}, false, err
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
			return Ticket{}, false, err
		}
		return t, true, nil
	}
	return Ticket{}, false, nil
}

// writeWalkIn reopens the holder's own cancelled Ticket (found), else writes
// the guest Ticket of w, under the capacity without its waiting list. A
// Ticket written by another request since the read is answered existing.
func (s *service) writeWalkIn(ctx context.Context, ev event.Event, w WalkIn, found Ticket, ok bool) (Ticket, WalkInResult, error) {
	if ok {
		reopened, changed, err := s.reopenIfCancelled(ctx, found, Admission{})
		if err != nil {
			return Ticket{}, "", err
		}
		if !changed {
			return reopened, WalkInExisting, nil
		}
		return reopened, WalkInCreated, nil
	}
	created, err := s.create(ctx, Ticket{
		EventID:          ev.ID,
		TicketType:       Guest,
		GuestFirstName:   w.FirstName,
		GuestLastName:    w.LastName,
		GuestEmail:       w.Email,
		GuestPhoneNumber: w.PhoneNumber,
		GuestUniversity:  w.University,
		GuestDepartment:  w.Department,
		GuestGrade:       w.Grade,
	}, Admission{})
	if errors.Is(err, ErrConflict) {
		again, getErr := s.tickets.GetByGuestEvent(ctx, w.Email, ev.ID)
		if getErr != nil {
			return Ticket{}, "", getErr
		}
		if !again.Active() {
			return Ticket{}, "", inactiveErr(again)
		}
		return again, WalkInExisting, nil
	}
	if err != nil {
		return Ticket{}, "", err
	}
	return created, WalkInCreated, nil
}

// walkInEntry is the answer for t, with the day's entry written in the
// venue Entry mode (source walk_in for a Ticket the Walk-in wrote, desk for
// one found).
func (s *service) walkInEntry(ctx context.Context, p authz.Principal, ev event.Event, t Ticket, result WalkInResult, mail bool) (WalkInOutcome, error) {
	name, _ := s.doorTicketIdentity(ctx, t)
	out := WalkInOutcome{Result: result, Name: name, TicketCode: t.Code, TicketMail: mail}
	if ev.EffectiveEntryMode != event.EntryVenue {
		return out, nil
	}
	source := EntryDesk
	if result == WalkInCreated {
		source = EntryWalkIn
	}
	entry, err := s.enterEvent(ctx, ev, t, staffEntry(source, p))
	if err != nil {
		return WalkInOutcome{}, err
	}
	if out.Name == "" {
		out.Name = entry.Name
	}
	out.Entry = &entry
	return out, nil
}

// createdNotifier is a store that queues the new Ticket's mail
// (PostgresStore while TICKET_CREATED_NOTIFICATION is on).
type createdNotifier interface {
	NotifiesCreated() bool
}

// createdMailOn reports whether a Ticket written now queues its mail.
func (s *service) createdMailOn() bool {
	n, ok := s.tickets.(createdNotifier)
	return ok && n.NotifiesCreated()
}

// beginWalkInAudit opens the Team audit row of a Walk-in against the Event's
// team: privileged, else the Leader of a Team the Event's team (or one of
// its Co-organizer teams) sits in or under, else the door staff through the
// Event's team (BasisDoorStaff). The caller has passed the door's rule.
// Without an audit writer nothing is recorded (a test).
func (s *service) beginWalkInAudit(ctx context.Context, p authz.Principal, ev event.Event) (*auditRecord, error) {
	if s.audit == nil {
		return nil, nil
	}
	actor, ok := teamaudit.ActorFrom(ctx)
	if !ok || s.teams == nil {
		return nil, teamaudit.ErrUnavailable
	}
	team, err := s.teams.GetGroup(ctx, ev.OwnerTeam)
	if err != nil {
		return nil, teamaudit.ErrUnavailable
	}
	actor.Basis, actor.ViaGroupID = teamaudit.BasisPrivileged, ""
	if !s.privilegedOperator(p) {
		via, err := s.leaderVia(ctx, p, team)
		if errors.Is(err, ErrForbidden) {
			via, err = s.coLeaderVia(ctx, p, ev)
		}
		switch {
		case err == nil:
			actor.Basis, actor.ViaGroupID = teamaudit.BasisLeader, via
		case errors.Is(err, ErrForbidden):
			actor.Basis, actor.ViaGroupID = teamaudit.BasisDoorStaff, team.ID
		default:
			return nil, teamaudit.ErrUnavailable
		}
	}
	after := teamaudit.State{EventID: ev.ID.String()}
	id, err := s.audit.Begin(ctx, teamaudit.Record{
		Actor: actor, Action: teamaudit.ActionTicketWalkIn, GroupID: team.ID, GroupPath: team.Path, After: &after,
	})
	if err != nil {
		return nil, err
	}
	return &auditRecord{id: id, after: after}, nil
}

// finishWalkInAudit writes the row's outcome: ok with the Ticket written, or
// failed with a code naming nobody. A Ticket another request wrote since
// the read (a double submit, result existing) is not this Walk-in's: the
// row is closed failed, existing, without it.
func (s *service) finishWalkInAudit(ctx context.Context, record *auditRecord, ticketID uuid.UUID, result WalkInResult, err error) {
	if record == nil {
		return
	}
	outcome, code := teamaudit.OutcomeOK, ""
	after := record.after
	switch {
	case err != nil:
		outcome = teamaudit.OutcomeFailed
		switch {
		case errors.Is(err, ErrEventFull):
			code = walkInAuditEventFull
		case errors.Is(err, ErrNotFound):
			code = walkInAuditNotFound
		case errors.Is(err, ErrTicketCancelled), errors.Is(err, ErrTicketWaitlisted):
			code = walkInAuditInvalidState
		default:
			code = walkInAuditFailed
		}
	case result == WalkInExisting:
		outcome, code = teamaudit.OutcomeFailed, walkInAuditExisting
	case ticketID != uuid.Nil:
		after.TicketID = ticketID.String()
	}
	if ferr := s.audit.Finish(ctx, record.id, outcome, code, &after); ferr != nil {
		s.logger.Printf("tickets: audit record %s stays pending", record.id)
	}
}
