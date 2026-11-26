package formsfeed

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/subjectlock"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// Outcomes of an answer the Unticketed respondents took
// (docs/form-respondents.md), besides the Ticket rule's.
const (
	// OutcomeRespondentKept is an answer kept as an Unticketed respondent
	// and no Ticket written: pending in Forms, or accepted on an Event
	// whose "Otomatik bilet gönder" is off.
	OutcomeRespondentKept = "respondent_kept"
)

// RespondentTicketer writes an Unticketed respondent's Ticket by the one
// Ticket rule (ticket.Service.AdmitFormRespondent).
type RespondentTicketer interface {
	AdmitFormRespondent(ctx context.Context, eventID uuid.UUID, r ticket.FormRespondent) (ticket.FormRespondentOutcome, error)
	KeepFormRespondent(ctx context.Context, eventID uuid.UUID, r ticket.FormRespondent) error
	EnsureFormRespondent(ctx context.Context, userID uuid.UUID) (bool, error)
	BeginFormRespondentSend(ctx context.Context, p authz.Principal, eventID uuid.UUID) (*ticket.FormRespondentSend, error)
	FinishFormRespondentJob(ctx context.Context, eventID, jobID uuid.UUID, sum ticket.FormRespondentSummary, err error)
}

// CapacityFunc reads an Event's capacity now (event.ReadCapacity).
type CapacityFunc func(ctx context.Context, eventID uuid.UUID) (event.Capacity, error)

// CollectingReader reports whether a gathering of the Event's form (of any
// of its forms when formID is nil) waits or runs (PostgresPulls).
type CollectingReader interface {
	Collecting(ctx context.Context, eventID uuid.UUID, formID *uuid.UUID) (bool, error)
}

// Respondents are the Unticketed respondents (docs/form-respondents.md;
// hub SPEC form-alan-eslestirme §6a, §7): the way every answer of a linked
// form goes between the Ticket rule and a respondent row (Route), and the
// panel's routes (List, Preview, Send, Job). Store and Events are required;
// the routes need Authz, Tickets and Jobs too.
type Respondents struct {
	Store  RespondentStore
	Events BackfillEvents
	Authz  authz.Authorizer
	// Tickets writes a respondent's Ticket.
	Tickets RespondentTicketer
	// Jobs queues "send them all" (all: true).
	Jobs TicketJobQueue
	// Pulls says whether a gathering runs (the list's collecting, a job's
	// wait); nil says none does.
	Pulls CollectingReader
	// Capacity reads the Event's capacity for the preview; nil counts
	// every candidate as a Ticket.
	Capacity CapacityFunc
	// People reads signed-in respondents' names and e-mails from core's
	// accounts for the list (user.Store.Accounts); nil leaves them empty.
	// A row keeps none of them.
	People func(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]user.Account, error)
	Now    func() time.Time
	Logf   func(string, ...any)
}

func (s *Respondents) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Respondents) logf(format string, args ...any) {
	if s.Logf != nil {
		s.Logf(format, args...)
	}
}

// log writes one "form_respondent" line: ids, the step and its outcome,
// never who answered.
func (s *Respondents) log(step string, eventID, formID uuid.UUID, outcome string, count int) {
	payload, err := json.Marshal(struct {
		Event   string `json:"event"`
		Step    string `json:"step"`
		EventID string `json:"event_id"`
		FormID  string `json:"form_id"`
		Outcome string `json:"outcome"`
		Count   int    `json:"count"`
	}{"form_respondent", step, eventID.String(), formID.String(), outcome, count})
	if err == nil {
		s.logf("%s", payload)
	}
}

// openEvents are the current Events that list the form, neither cancelled
// nor ended: where an answer may still earn a Ticket.
func (s *Respondents) openEvents(ctx context.Context, formID uuid.UUID) ([]event.Event, error) {
	evs, err := s.Events.ListIncludingDrafts(ctx, "")
	if err != nil {
		return nil, err
	}
	now := s.now()
	var out []event.Event
	for _, ev := range evs {
		if ev.ArchivedAt == nil && ev.Status != event.StatusCancelled && !ev.EndedAt(now) && ev.ListsForm(formID) {
			out = append(out, ev)
		}
	}
	return out, nil
}

// autoTicket is the Event's "Otomatik bilet gönder": on unless set off.
func autoTicket(ev event.Event) bool {
	return ev.FormResponsesAutoTicket == nil || *ev.FormResponsesAutoTicket
}

// appliesTo reports whether the respondent may be written on the Event: a
// mapped guest on the Events whose mapping built it, anyone else on every
// Event listing the form.
func (r Respondent) appliesTo(eventID uuid.UUID) bool {
	return r.Events == nil || slices.Contains(r.Events, eventID)
}

// respondentRow is the row the respondent keeps on the Event; false for
// nobody, or a guest without a usable name or address.
func respondentRow(r Respondent, eventID, formID, responseID uuid.UUID, submittedAt *time.Time, status, source string) (RespondentRow, bool) {
	row := RespondentRow{EventID: eventID, FormID: formID, ResponseID: responseID, SubmittedAt: submittedAt, FormsStatus: status, Source: source}
	switch {
	case r.UserID != nil && *r.UserID != uuid.Nil:
		row.UserID = r.UserID
		return row, true
	case r.Guest != nil:
		first, last := cleanName(r.Guest.FirstName), cleanName(r.Guest.LastName)
		email := strings.ToLower(strings.TrimSpace(r.Guest.Email))
		if first == "" || last == "" || utf8.RuneCountInString(first) > maxMappedName || utf8.RuneCountInString(last) > maxMappedName || !plainAddress(email) {
			return RespondentRow{}, false
		}
		row.FirstName, row.LastName, row.Email = first, last, email
		return row, true
	}
	return RespondentRow{}, false
}

// Route is the way of an answer whose respondents ResolveRespondents found,
// whichever way it came (the HTTP report, a core.forms event, a pull), once
// the Unticketed respondents are on (hub SPEC §6a.3):
//
//   - declined: the answer's rows go; the Ticket rule marks its Tickets
//     (K6).
//   - pending: the Ticket rule marks its Tickets pending again; on each
//     open Event listing the form, a person without a Ticket there is kept
//     as a pending row, whatever "Otomatik bilet gönder" says.
//   - accepted: the Ticket rule writes the Tickets on the open Events whose
//     "Otomatik bilet gönder" is on, as before; then on every open Event a
//     person still without a Ticket there (the setting off, the Event full,
//     applying closed) is kept as an accepted row, and a row of someone now
//     holding one goes.
//
// A nil Respondents is the rule alone (RecordRespondents), as before.
func (s *Respondents) Route(ctx context.Context, record RecordFunc, report ticket.FormResponse, respondents []Respondent) (ticket.FormResponseResult, error) {
	if s == nil {
		return RecordRespondents(ctx, record, report, respondents)
	}
	switch report.Status {
	case ticket.FormResponseDeclined:
		dropped, err := s.Store.DropAnswer(ctx, report.ResponseID, nil)
		if err != nil {
			return ticket.FormResponseResult{}, err
		}
		if dropped > 0 {
			s.log("route", uuid.Nil, report.FormID, "declined_dropped", dropped)
		}
		return RecordRespondents(ctx, record, report, respondents)
	case ticket.FormResponsePending, ticket.FormResponseAccepted:
	default:
		return RecordRespondents(ctx, record, report, respondents)
	}
	evs, err := s.openEvents(ctx, report.FormID)
	if err != nil {
		return ticket.FormResponseResult{}, err
	}
	var on, off []uuid.UUID
	for _, ev := range evs {
		if autoTicket(ev) {
			on = append(on, ev.ID)
		} else {
			off = append(off, ev.ID)
		}
	}
	var result ticket.FormResponseResult
	ruled := report.Status == ticket.FormResponsePending || len(on) > 0 || len(evs) == 0
	if ruled {
		withOn := report
		if report.Status == ticket.FormResponseAccepted && len(off) > 0 {
			withOn.OnlyEvents = on
		}
		if result, err = RecordRespondents(ctx, record, withOn, respondents); err != nil {
			return result, err
		}
	}
	if result.Outcome == ticket.FormResponsePersonUnavailable || result.Outcome == ticket.FormResponseFeedback {
		return result, nil
	}
	status := RespondentPending
	if report.Status == ticket.FormResponseAccepted {
		status = RespondentAccepted
	}
	kept := 0
	for _, ev := range evs {
		for _, r := range respondents {
			if !r.appliesTo(ev.ID) {
				continue
			}
			row, ok := respondentRow(r, ev.ID, report.FormID, report.ResponseID, report.SubmittedAt, status, SourceNew)
			if !ok {
				continue
			}
			if report.Status == ticket.FormResponseAccepted && !autoTicket(ev) && s.Tickets != nil {
				// The rule did not run on this Event: a Ticket the person
				// holds here takes the accepted answer as the rule would
				// (Forms' rejection cleared, a Returned answer taken over).
				if err := s.Tickets.KeepFormRespondent(ctx, ev.ID, rowRespondent(row)); err != nil {
					return result, err
				}
			}
			got, err := s.keep(ctx, row)
			if err != nil {
				return result, err
			}
			if got {
				kept++
			}
		}
	}
	switch {
	case kept > 0:
		s.log("route", uuid.Nil, report.FormID, status+"_kept", kept)
		if !ruled || report.Status == ticket.FormResponsePending {
			result.Outcome = ticket.FormResponseOutcome(OutcomeRespondentKept)
		}
	case !ruled:
		// Every Event has the setting off and the person holds a Ticket on
		// each already (or names nobody usable): nothing new.
		result.Outcome = ticket.FormResponseRecorded
	}
	return result, nil
}

// rowRespondent is the row's person as the Ticket rule takes them.
func rowRespondent(row RespondentRow) ticket.FormRespondent {
	r := ticket.FormRespondent{FormID: row.FormID, ResponseID: row.ResponseID, UserID: row.UserID}
	if row.UserID == nil {
		r.Guest = &ticket.GuestInfo{FirstName: row.FirstName, LastName: row.LastName, Email: row.Email}
	}
	return r
}

// keep keeps the row (RespondentStore.Keep); a signed-in person core cannot
// find, or whose account is blocked or erased, is not kept (the row names
// only an active account, as the rule's Ticket does).
func (s *Respondents) keep(ctx context.Context, row RespondentRow) (bool, error) {
	if row.UserID != nil && s.Tickets != nil {
		ok, err := s.Tickets.EnsureFormRespondent(ctx, *row.UserID)
		if err != nil || !ok {
			return false, err
		}
	}
	kept, err := s.Store.Keep(ctx, row)
	if subjectlock.IsInactiveAccountReference(err) {
		return false, nil
	}
	return kept, err
}

// Refusals of the respondent routes (the handler's problem codes).
var (
	// ErrRespondentsInvalid is a body that is neither 1–200 ids nor
	// all: true (400 respondents_invalid).
	ErrRespondentsInvalid = errors.New("formsfeed: invalid respondent selection")
	// ErrRespondentsEventCancelled and ErrRespondentsEventEnded are an
	// Event that takes no Ticket now (409 event_cancelled, event_ended).
	ErrRespondentsEventCancelled = errors.New("formsfeed: the Event is cancelled")
	ErrRespondentsEventEnded     = errors.New("formsfeed: the Event has ended")
	// ErrTicketJobNotFound is no such job of the Event (404).
	ErrTicketJobNotFound = errors.New("formsfeed: no such ticket job")
)

// MaxRespondentIDs bounds a request's ids: more go as all: true, a job.
const MaxRespondentIDs = 200

// Selection is which Unticketed respondents a preview or a send covers:
// IDs (1 to MaxRespondentIDs), or All of the Event's (of FormID when set).
type Selection struct {
	IDs    []uuid.UUID
	All    bool
	FormID *uuid.UUID
}

func (sel Selection) valid() bool {
	if sel.All {
		return len(sel.IDs) == 0
	}
	return len(sel.IDs) > 0 && len(sel.IDs) <= MaxRespondentIDs
}

// operable reads the Event and checks the caller may do action on it
// (authz on the Event, its Co-organizers included, as the Event's Ticket
// routes; #456): for the list Read on its Tickets; for the preview, the
// sending and its job (action Assign) the attendee import's rule
// (ticket operatorOf): Update on the Event or Assign on its Tickets. A
// Co-organizer team that only lets its members see the Event sends no
// Ticket. Never a product's service account. A draft the caller may not
// see is ErrMappingEventNotFound.
func (s *Respondents) operable(ctx context.Context, p authz.Principal, eventID uuid.UUID, action authz.Action) (event.Event, error) {
	ev, err := s.Events.Get(ctx, eventID)
	if errors.Is(err, event.ErrNotFound) {
		return event.Event{}, ErrMappingEventNotFound
	}
	if err != nil {
		return event.Event{}, err
	}
	if ev.ArchivedAt != nil {
		return event.Event{}, ErrMappingEventNotFound
	}
	if !s.allowed(p, ev, action) {
		if s.Authz == nil || !event.VisibleTo(s.Authz, &p, ev) {
			return event.Event{}, ErrMappingEventNotFound
		}
		return event.Event{}, ErrMappingForbidden
	}
	return ev, nil
}

func (s *Respondents) allowed(p authz.Principal, ev event.Event, action authz.Action) bool {
	if p.ServiceAccount || s.Authz == nil {
		return false
	}
	now := s.now()
	if action != authz.Assign {
		return s.Authz.Allow(p, event.AuthzResource(ev, authz.TypeTicket, now), action)
	}
	return p.Product == "" && p.ID != "" &&
		(s.Authz.Allow(p, event.AuthzResource(ev, authz.TypeEvent, now), authz.Update) ||
			s.Authz.Allow(p, event.AuthzResource(ev, authz.TypeTicket, now), authz.Assign))
}

// ticketable refuses an Event that takes no Ticket now.
func (s *Respondents) ticketable(ev event.Event) error {
	switch {
	case ev.Status == event.StatusCancelled:
		return ErrRespondentsEventCancelled
	case ev.EndedAt(s.now()):
		return ErrRespondentsEventEnded
	}
	return nil
}

// Default and largest page of the list.
const (
	DefaultRespondentPage = 50
	MaxRespondentPage     = 200
)

// List is a page of the Event's Unticketed respondents, and whether a
// gathering of its forms (of q's form) still runs.
func (s *Respondents) List(ctx context.Context, p authz.Principal, eventID uuid.UUID, q RespondentQuery) (RespondentPage, bool, error) {
	if _, err := s.operable(ctx, p, eventID, authz.Read); err != nil {
		return RespondentPage{}, false, err
	}
	switch q.FormsStatus {
	case "", RespondentAccepted, RespondentPending:
	default:
		return RespondentPage{}, false, ErrRespondentsInvalid
	}
	if q.Limit <= 0 {
		q.Limit = DefaultRespondentPage
	}
	q.Limit = min(q.Limit, MaxRespondentPage)
	if utf8.RuneCountInString(q.Q) > 100 {
		return RespondentPage{}, false, ErrRespondentsInvalid
	}
	page, err := s.Store.List(ctx, eventID, q)
	if errors.Is(err, ErrInvalidCursor) {
		return RespondentPage{}, false, ErrRespondentsInvalid
	}
	if err != nil {
		return RespondentPage{}, false, err
	}
	if err := s.withPeople(ctx, page.Items); err != nil {
		return RespondentPage{}, false, err
	}
	collecting := false
	if s.Pulls != nil {
		if collecting, err = s.Pulls.Collecting(ctx, eventID, q.FormID); err != nil {
			return RespondentPage{}, false, err
		}
	}
	return page, collecting, nil
}

// withPeople fills a signed-in respondent's name and e-mail from core's
// account, for the answer only; an erased or blocked person shows none.
func (s *Respondents) withPeople(ctx context.Context, rows []RespondentRow) error {
	if s.People == nil {
		return nil
	}
	var ids []uuid.UUID
	for _, r := range rows {
		if r.UserID != nil {
			ids = append(ids, *r.UserID)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	accounts, err := s.People(ctx, ids)
	if err != nil {
		return err
	}
	for i := range rows {
		if rows[i].UserID == nil {
			continue
		}
		if a, ok := accounts[*rows[i].UserID]; ok && a.Stored && !a.Blocked {
			rows[i].FirstName, rows[i].LastName, rows[i].Email = a.User.FirstName, a.User.LastName, a.User.Email
		}
	}
	return nil
}

// Preview is what sending the selection would do now, by the Event's
// capacity: how many get an active Ticket, how many go on the waiting list,
// how many find no place (full, the waiting list off), how many hold a
// Ticket already, and how many wait for Forms' review (never sent).
type Preview struct {
	Tickets         int `json:"tickets"`
	Waitlisted      int `json:"waitlisted"`
	Full            int `json:"full"`
	AlreadyTicketed int `json:"alreadyTicketed"`
	PendingSkipped  int `json:"pendingSkipped"`
}

// Preview counts what Send would do with the selection now. A count, not a
// promise: a registration in between takes a place too.
func (s *Respondents) Preview(ctx context.Context, p authz.Principal, eventID uuid.UUID, sel Selection) (Preview, error) {
	ev, err := s.operable(ctx, p, eventID, authz.Assign)
	if err != nil {
		return Preview{}, err
	}
	if !sel.valid() {
		return Preview{}, ErrRespondentsInvalid
	}
	if err := s.ticketable(ev); err != nil {
		return Preview{}, err
	}
	candidates, ticketed, pending, err := s.Store.Counts(ctx, eventID, sel.IDs, sel.All, sel.FormID)
	if err != nil {
		return Preview{}, err
	}
	out := Preview{Tickets: candidates, AlreadyTicketed: ticketed, PendingSkipped: pending}
	if s.Capacity == nil {
		return out, nil
	}
	c, err := s.Capacity(ctx, eventID)
	if err != nil {
		return Preview{}, err
	}
	if free := c.Free(); free >= 0 && candidates > free {
		out.Tickets = free
		if c.Waitlist {
			out.Waitlisted = candidates - free
		} else {
			out.Full = candidates - free
		}
	}
	return out, nil
}

// SendResult counts a send: new active Tickets, waiting list places, people
// who found no place (they stay), and the rest skipped (pending in Forms,
// holding a Ticket already, gone, or not the Event's).
type SendResult struct {
	Recorded   int `json:"recorded"`
	Waitlisted int `json:"waitlisted"`
	Full       int `json:"full"`
	Skipped    int `json:"skipped"`
}

func (r *SendResult) add(o SendResult) {
	r.Recorded += o.Recorded
	r.Waitlisted += o.Waitlisted
	r.Full += o.Full
	r.Skipped += o.Skipped
}

// sent is what send did, with the audit row's counts.
type sent struct {
	SendResult
	// handled is how many of the rows send finished, in order: on an
	// error, the rows before it (a job goes on after them).
	handled int
	audit   ticket.FormRespondentSummary
}

// send writes the Tickets of rows one by one by the one Ticket rule. A row
// whose person holds a Ticket after it (written now or before) goes; a
// full Event's stays; a pending row is skipped and stays.
func (s *Respondents) send(ctx context.Context, eventID uuid.UUID, rows []RespondentRow) (sent, error) {
	var res sent
	for i, row := range rows {
		res.handled = i
		res.audit.Rows++
		if row.UserID != nil {
			res.audit.Members++
		} else {
			res.audit.Guests++
		}
		if row.FormsStatus != RespondentAccepted {
			res.Skipped++
			continue
		}
		outcome, err := s.Tickets.AdmitFormRespondent(ctx, eventID, rowRespondent(row))
		switch {
		case errors.Is(err, ticket.ErrEventCancelled):
			return res, ErrRespondentsEventCancelled
		case errors.Is(err, ticket.ErrEventEnded):
			return res, ErrRespondentsEventEnded
		case errors.Is(err, ticket.ErrNotFound):
			return res, ErrMappingEventNotFound
		case err != nil:
			return res, err
		}
		switch outcome {
		case ticket.FormRespondentFull:
			res.Full++
			res.audit.Full++
			continue
		case ticket.FormRespondentRecorded:
			res.Recorded++
			res.audit.Recorded++
		case ticket.FormRespondentWaitlisted:
			res.Waitlisted++
			res.audit.Waitlisted++
		default:
			res.Skipped++
			res.audit.Existing++
		}
		if err := s.Store.Delete(ctx, row.ID); err != nil {
			// The Ticket is written: the row is handled, its count kept.
			res.handled = i + 1
			return res, err
		}
	}
	res.handled = len(rows)
	return res, nil
}

// Send writes the selection's Tickets: up to MaxRespondentIDs ids now, all
// of them as a background job (its id; one waiting job an Event and form,
// so the same request again finds it). Sending the same ids again writes
// no second Ticket: their rows went with the first. Each request writes a
// Team audit row (ticket.import, as an attendee import); with the record
// on and no row, it is refused (teamaudit.ErrUnavailable) before anything
// is written.
func (s *Respondents) Send(ctx context.Context, p authz.Principal, eventID uuid.UUID, sel Selection) (SendResult, *uuid.UUID, error) {
	ev, err := s.operable(ctx, p, eventID, authz.Assign)
	if err != nil {
		return SendResult{}, nil, err
	}
	if !sel.valid() {
		return SendResult{}, nil, ErrRespondentsInvalid
	}
	if err := s.ticketable(ev); err != nil {
		return SendResult{}, nil, err
	}
	audit, err := s.Tickets.BeginFormRespondentSend(ctx, p, eventID)
	if err != nil {
		return SendResult{}, nil, err
	}
	if sel.All {
		var by *uuid.UUID
		if id, err := uuid.Parse(p.ID); err == nil {
			by = &id
		}
		// The job takes the row's id: the row stays pending while it runs,
		// and its end writes the job's own counts there
		// (FinishFormRespondentJob). The same request while a job waits
		// finds that job: its row is closed now, naming it.
		jobID := audit.ID()
		if jobID == uuid.Nil {
			jobID = uuid.New()
		}
		id, err := s.Jobs.Enqueue(ctx, jobID, eventID, sel.FormID, by, s.now().UTC())
		if err != nil {
			audit.Finish(ctx, ticket.FormRespondentSummary{}, err)
			return SendResult{}, nil, err
		}
		if id != jobID {
			audit.Finish(ctx, ticket.FormRespondentSummary{JobID: &id}, nil)
		}
		formID := uuid.Nil
		if sel.FormID != nil {
			formID = *sel.FormID
		}
		s.log("send_all", eventID, formID, "queued", 0)
		return SendResult{}, &id, nil
	}
	ids := slices.Clone(sel.IDs)
	slices.SortFunc(ids, func(a, b uuid.UUID) int { return strings.Compare(a.String(), b.String()) })
	ids = slices.Compact(ids)
	rows, err := s.Store.ByIDs(ctx, eventID, ids)
	if err != nil {
		audit.Finish(ctx, ticket.FormRespondentSummary{}, err)
		return SendResult{}, nil, err
	}
	res, err := s.send(ctx, eventID, rows)
	res.Skipped += len(ids) - len(rows)
	audit.Finish(ctx, res.audit, err)
	s.log("send", eventID, uuid.Nil, "done", res.Recorded+res.Waitlisted)
	return res.SendResult, nil, err
}

// Job reads one of the Event's ticket jobs.
func (s *Respondents) Job(ctx context.Context, p authz.Principal, eventID, jobID uuid.UUID) (TicketJob, error) {
	if _, err := s.operable(ctx, p, eventID, authz.Assign); err != nil {
		return TicketJob{}, err
	}
	return s.Jobs.Get(ctx, eventID, jobID)
}
