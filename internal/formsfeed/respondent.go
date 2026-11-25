package formsfeed

import (
	"context"
	"errors"
	"net/mail"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
)

// Who an answer names, in this order (hub SPEC form-alan-eslestirme §6):
// the person who answered signed in, else the guest Forms names from the
// form's identity fields, else a guest built from an Event's form mapping.
const (
	RespondentUser       = "user"
	RespondentFormsGuest = "forms_guest"
	RespondentMapped     = "mapping"
	// RespondentNobody is an answer that names nobody and has no mapping:
	// the Ticket rule answers no_respondent, as before mappings.
	RespondentNobody = "nobody"
)

// Respondent is who an answer names: what the Ticket rule writes a Ticket
// for. Resolving it (ResolveRespondents) and writing the Ticket
// (RecordRespondents) are apart, so that a later step may keep the
// respondent and write the Ticket afterwards (hub bilet 04).
type Respondent struct {
	Source string
	UserID *uuid.UUID
	Guest  *ticket.GuestInfo
	// Events are, for a mapped guest, the Events whose mapping built it:
	// the only ones the guest is written on. Nil otherwise.
	Events []uuid.UUID
}

// Bounds of a mapped guest's details (the answers are an anonymous form
// filler's).
const (
	maxMappedName  = 100
	maxMappedEmail = 254
)

// ResolveRespondents is the one decision of who an answer names, for every
// way an answer comes in (the HTTP report, a core.forms event, a pull):
//
//   - a userId: that person;
//   - else Forms' named guest: that guest, as Forms sent it (the Ticket rule
//     checks it, as before);
//   - else, with mappings (those of the Events listing the form, for an
//     accepted answer): a guest built from each mapping's answers, the
//     Events of the same guest together;
//   - else nobody: the Ticket rule answers no_respondent.
//
// With mappings of which none gives a guest, it answers no respondent and
// FormResponseMappingIncomplete. answers are the answers Forms sent for
// the mappings' questions (fields).
func ResolveRespondents(userID *uuid.UUID, guest *Guest, answers map[string]string, mappings []Mapping) ([]Respondent, ticket.FormResponseOutcome) {
	switch {
	case userID != nil:
		return []Respondent{{Source: RespondentUser, UserID: userID}}, ""
	case guest != nil:
		return []Respondent{{Source: RespondentFormsGuest, Guest: &ticket.GuestInfo{FirstName: guest.FirstName, LastName: guest.LastName, Email: guest.Email}}}, ""
	case len(mappings) == 0:
		return []Respondent{{Source: RespondentNobody}}, ""
	}
	var out []Respondent
	for _, m := range mappings {
		g, ok := MappedGuest(m, answers)
		if !ok {
			continue
		}
		joined := false
		for i := range out {
			if *out[i].Guest == g {
				out[i].Events = append(out[i].Events, m.EventID)
				joined = true
				break
			}
		}
		if !joined {
			out = append(out, Respondent{Source: RespondentMapped, Guest: &g, Events: []uuid.UUID{m.EventID}})
		}
	}
	if len(out) == 0 {
		return nil, ticket.FormResponseMappingIncomplete
	}
	return out, ""
}

// MappedGuest builds a guest from a mapping's answers (hub SPEC §5.3):
//
//   - the e-mail trimmed and in lower case; not one plain address with a
//     dotted domain, no guest;
//   - the first and last name questions' answers, trimmed; a full name
//     question fills only what they leave empty (splitFullName);
//   - without a first and a last name, no guest.
//
// K4 (Yusuf, 2026-11-25, as proposed): an e-mail that is a member's account's
// is still a guest, as every guest Ticket is; matching it to the member is
// later work. This is the one place it would change.
func MappedGuest(m Mapping, answers map[string]string) (ticket.GuestInfo, bool) {
	email := strings.ToLower(strings.TrimSpace(answers[m.Email]))
	if m.Email == "" || !plainAddress(email) {
		return ticket.GuestInfo{}, false
	}
	var first, last string
	if m.FirstName != "" {
		first = cleanName(answers[m.FirstName])
	}
	if m.LastName != "" {
		last = cleanName(answers[m.LastName])
	}
	if m.FullName != "" && (first == "" || last == "") {
		if f, l, ok := splitFullName(cleanName(answers[m.FullName])); ok {
			if first == "" {
				first = f
			}
			if last == "" {
				last = l
			}
		}
	}
	if first == "" || last == "" || utf8.RuneCountInString(first) > maxMappedName || utf8.RuneCountInString(last) > maxMappedName {
		return ticket.GuestInfo{}, false
	}
	return ticket.GuestInfo{FirstName: first, LastName: last, Email: email}, true
}

// splitFullName splits a full name at its spaces: the last word is the last
// name, the words before it the first name.
//
// K3 (Yusuf, 2026-11-25, as proposed): a single word gives no name, so the
// answer writes no Ticket and counts as mapping_incomplete. This is the one
// place it would change.
func splitFullName(full string) (first, last string, ok bool) {
	words := strings.Fields(full)
	if len(words) < 2 {
		return "", "", false
	}
	return strings.Join(words[:len(words)-1], " "), words[len(words)-1], true
}

// cleanName trims an answer and folds its inner spaces; an answer with a
// control character is no name.
func cleanName(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if strings.IndexFunc(s, unicode.IsControl) >= 0 || !utf8.ValidString(s) {
		return ""
	}
	return s
}

// plainAddress is one plain address with a dotted domain, as an imported
// attendee's (ticket.validImportEmail).
func plainAddress(address string) bool {
	if address == "" || len(address) > maxMappedEmail || strings.ContainsAny(address, " \t\r\n:,;<>\"") {
		return false
	}
	parsed, err := mail.ParseAddress(address)
	if err != nil || parsed.Address != address {
		return false
	}
	at := strings.LastIndexByte(address, '@')
	return at > 0 && strings.Contains(address[at+1:], ".") && !strings.HasSuffix(address, ".")
}

// RecordRespondents writes the Tickets the respondents earn by the one
// Ticket rule (record): report is the answer, its UserID and Guest set
// from each respondent. A user, Forms' guest or nobody is one call, as
// before. Mapped guests are one call each, limited to their Events
// (ticket.FormResponse.GuestEvents); the results add up, and a written
// Ticket's outcome is FormResponseMappedGuest.
func RecordRespondents(ctx context.Context, record RecordFunc, report ticket.FormResponse, respondents []Respondent) (ticket.FormResponseResult, error) {
	if len(respondents) == 1 && respondents[0].Source != RespondentMapped {
		report.UserID, report.Guest, report.GuestEvents = respondents[0].UserID, respondents[0].Guest, nil
		return record(ctx, report)
	}
	var total ticket.FormResponseResult
	var outcomes []ticket.FormResponseOutcome
	for _, r := range respondents {
		one := report
		one.UserID, one.Guest, one.GuestEvents = nil, r.Guest, r.Events
		result, err := record(ctx, one)
		if err != nil {
			return total, err
		}
		total.TicketsWritten += result.TicketsWritten
		total.TicketsCancelled += result.TicketsCancelled
		total.ListingEvents = max(total.ListingEvents, result.ListingEvents)
		outcomes = append(outcomes, result.Outcome)
	}
	switch {
	case len(outcomes) == 0:
		total.Outcome = ticket.FormResponseNoRespondent
	case contains(outcomes, ticket.FormResponseRecorded):
		total.Outcome = ticket.FormResponseMappedGuest
	case contains(outcomes, ticket.FormResponseWaitlisted):
		total.Outcome = ticket.FormResponseWaitlisted
	default:
		total.Outcome = outcomes[0]
	}
	return total, nil
}

func contains(outcomes []ticket.FormResponseOutcome, want ticket.FormResponseOutcome) bool {
	for _, o := range outcomes {
		if o == want {
			return true
		}
	}
	return false
}

// MappingStoreReader reads the mappings of a form (MappingStore).
type MappingStoreReader interface {
	ForForm(ctx context.Context, formID uuid.UUID) ([]Mapping, error)
}

// MappedGuests reads the mappings that apply to an answer, and serves the
// HTTP report (RecordReport). The core.forms events and the pulls take the
// same steps in Recorder.Response.
type MappedGuests struct {
	Store  MappingStoreReader
	Events EventLister
	// Forms reads an answer's mapped answers for the HTTP report; nil
	// leaves such a report as before (no_respondent).
	Forms Reader
}

// Mappings are the mappings of the form whose Event lists it now (not
// archived): a mapping left by a form the Event let go is not read.
func (m *MappedGuests) Mappings(ctx context.Context, formID uuid.UUID) ([]Mapping, error) {
	if m == nil || m.Store == nil {
		return nil, nil
	}
	rows, err := m.Store.ForForm(ctx, formID)
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	evs, err := m.Events.ListIncludingDrafts(ctx, "")
	if err != nil {
		return nil, err
	}
	listing := map[uuid.UUID]bool{}
	for _, ev := range evs {
		if ev.ArchivedAt == nil && ev.ListsForm(formID) {
			listing[ev.ID] = true
		}
	}
	var out []Mapping
	for _, row := range rows {
		if listing[row.EventID] {
			out = append(out, row)
		}
	}
	return out, nil
}

// MappedFields are the question ids the mappings ask for, each once.
func MappedFields(mappings []Mapping) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range mappings {
		for _, id := range m.Fields() {
			if !seen[id] {
				seen[id] = true
				out = append(out, id)
			}
		}
	}
	return out
}

// RecordReport is the HTTP report's way through the mappings: an accepted
// answer that names nobody, to a form an Event maps, is read from Forms'
// single answer endpoint with the mapped questions (fields), and its
// respondents are resolved and written as an event's or a pull's are. Any
// other report goes to the Ticket rule as it came. A read that finds
// nothing (404) is OutcomeGone, a 2xx; another failed read is an error
// (the report answers 5xx, Forms sends it again).
func (m *MappedGuests) RecordReport(ctx context.Context, record RecordFunc, r ticket.FormResponse) (ticket.FormResponseResult, error) {
	if m == nil || m.Forms == nil || r.Status != ticket.FormResponseAccepted || r.UserID != nil || r.Guest != nil || r.ResponseID == uuid.Nil {
		return record(ctx, r)
	}
	mappings, err := m.Mappings(ctx, r.FormID)
	if err != nil {
		return ticket.FormResponseResult{}, err
	}
	if len(mappings) == 0 {
		return record(ctx, r)
	}
	got, err := m.Forms.Response(ctx, r.FormID, r.ResponseID, MappedFields(mappings)...)
	switch {
	case errors.Is(err, ErrNotFound):
		return ticket.FormResponseResult{Outcome: ticket.FormResponseOutcome(OutcomeGone)}, nil
	case err != nil:
		return ticket.FormResponseResult{}, err
	}
	if got.Status != StatusAccepted {
		// Forms' answer moved on since the report; its own report follows.
		return record(ctx, r)
	}
	respondents, outcome := ResolveRespondents(got.UserID, got.Guest, got.Answers, mappings)
	if outcome != "" {
		return ticket.FormResponseResult{Outcome: outcome}, nil
	}
	return RecordRespondents(ctx, record, r, respondents)
}
