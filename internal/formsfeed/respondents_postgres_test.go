package formsfeed_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/formsfeed"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// respondentWorld is one PostgreSQL with an Event listing a form, the one
// Ticket rule, and the Unticketed respondents.
type respondentWorld struct {
	pool    *pgxpool.Pool
	events  *event.PostgresStore
	svc     ticket.Service
	rs      *formsfeed.Respondents
	pulls   *formsfeed.PostgresPulls
	ev      event.Event
	formID  uuid.UUID
	editor  authz.Principal
	reports *formsfeed.MappedGuests
	record  formsfeed.RecordFunc
}

func newRespondentWorld(t *testing.T, auto bool, capacity int, waitlist bool) respondentWorld {
	t.Helper()
	pool := migrated(t)
	ctx := context.Background()
	evStore := event.NewPostgresStore(pool)
	tickets := ticket.NewPostgresStore(pool)
	formID := uuid.New()
	start, end := time.Now().Add(24*time.Hour), time.Now().Add(26*time.Hour)
	ev, err := evStore.Create(ctx, event.Event{
		Name: "Hack", Location: "YTÜ", OwnerTeam: "WEBLAB", StartDate: &start, EndDate: &end,
		Capacity: capacity, WaitlistEnabled: &waitlist, FormResponsesAutoTicket: &auto,
		FormURL: "https://forms.yildizskylab.com/" + formID.String(),
	})
	if err != nil {
		t.Fatal(err)
	}
	az := authz.NewAuthorizer(authz.DefaultPolicy())
	svc := ticket.NewService(tickets, evStore, az, user.Store(user.NewPostgresStore(pool)))
	pulls := formsfeed.NewPostgresPulls(pool)
	rs := &formsfeed.Respondents{
		Store: formsfeed.NewPostgresRespondents(pool), Events: evStore, Authz: az, Tickets: svc,
		Jobs: formsfeed.NewPostgresTicketJobs(pool), Pulls: pulls,
		Capacity: func(ctx context.Context, id uuid.UUID) (event.Capacity, error) {
			return event.ReadCapacity(ctx, pool, id)
		},
	}
	record := func(ctx context.Context, r ticket.FormResponse) (ticket.FormResponseResult, error) {
		return svc.RecordFormResponse(ctx, formsService, r)
	}
	return respondentWorld{
		pool: pool, events: evStore, svc: svc, rs: rs, pulls: pulls, ev: ev, formID: formID,
		editor:  authz.Principal{ID: uuid.NewString(), Groups: []string{"/UYELER/YK"}},
		reports: &formsfeed.MappedGuests{Store: formsfeed.NewPostgresMappings(pool), Events: evStore, Respondents: rs},
		record:  record,
	}
}

func (w respondentWorld) report(t *testing.T, responseID uuid.UUID, status ticket.FormResponseStatus, email string) ticket.FormResponseResult {
	t.Helper()
	got, err := w.reports.RecordReport(context.Background(), w.record, ticket.FormResponse{
		FormID: w.formID, ResponseID: responseID, Status: status,
		Guest: &ticket.GuestInfo{FirstName: "Ada", LastName: "Lovelace", Email: email},
	})
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func (w respondentWorld) count(t *testing.T, query string, args ...any) int {
	t.Helper()
	var n int
	if err := w.pool.QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func (w respondentWorld) rows(t *testing.T) []formsfeed.RespondentRow {
	t.Helper()
	page, _, err := w.rs.List(context.Background(), w.editor, w.ev.ID, formsfeed.RespondentQuery{})
	if err != nil {
		t.Fatal(err)
	}
	return page.Items
}

// SPEC §6a.3, "Otomatik bilet gönder" on (the default): an accepted answer
// writes its Ticket as before; a pending one is kept ("Forms'ta onay
// bekliyor") and its acceptance writes the Ticket and drops the row; the
// holder's answer back to pending keeps the Ticket, marked; a decline marks
// it (K6) and drops a row.
func TestTheDecisionTableWithAutoTicketOn(t *testing.T) {
	w := newRespondentWorld(t, true, 0, true)
	first, second := uuid.New(), uuid.New()

	if got := w.report(t, first, ticket.FormResponseAccepted, "ada@example.com"); got.TicketsWritten != 1 {
		t.Fatalf("accepted: %+v", got)
	}
	if n := len(w.rows(t)); n != 0 {
		t.Fatalf("an accepted answer with its Ticket left %d rows", n)
	}

	if got := w.report(t, second, ticket.FormResponsePending, "grace@example.com"); got.Outcome != formsfeed.OutcomeRespondentKept {
		t.Fatalf("pending: %+v", got)
	}
	rows := w.rows(t)
	if len(rows) != 1 || rows[0].FormsStatus != formsfeed.RespondentPending || rows[0].Source != formsfeed.SourceNew || rows[0].Email != "grace@example.com" {
		t.Fatalf("pending rows %+v", rows)
	}
	if got := w.report(t, second, ticket.FormResponseAccepted, "grace@example.com"); got.TicketsWritten != 1 {
		t.Fatalf("pending → accepted: %+v", got)
	}
	if n := len(w.rows(t)); n != 0 {
		t.Fatalf("the accepted answer's row stayed (%d)", n)
	}

	// Accepted → pending (the reviewer took it back): the Ticket stays,
	// marked; accepted again clears the mark.
	w.report(t, first, ticket.FormResponsePending, "ada@example.com")
	if n := w.count(t, `SELECT count(*) FROM tickets WHERE form_response_id = $1 AND form_response_status = 'pending' AND status = 'active'`, first); n != 1 {
		t.Fatalf("pending mark %d", n)
	}
	if n := len(w.rows(t)); n != 0 {
		t.Fatalf("a ticket holder was kept as a respondent (%d)", n)
	}
	w.report(t, first, ticket.FormResponseAccepted, "ada@example.com")
	if n := w.count(t, `SELECT count(*) FROM tickets WHERE form_response_id = $1 AND form_response_status = 'pending'`, first); n != 0 {
		t.Fatalf("the pending mark stayed %d", n)
	}
	listed, err := w.svc.ListByEventStatus(context.Background(), w.editor, w.ev.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tk := range listed {
		if tk.FormsStatus != "accepted" {
			t.Fatalf("formsStatus %q", tk.FormsStatus)
		}
	}

	// A declined pending answer's row goes; a declined Ticket's answer
	// marks the Ticket (K6).
	third := uuid.New()
	w.report(t, third, ticket.FormResponsePending, "linus@example.com")
	w.report(t, third, ticket.FormResponseDeclined, "linus@example.com")
	if n := len(w.rows(t)); n != 0 {
		t.Fatalf("the declined answer's row stayed (%d)", n)
	}
	w.report(t, first, ticket.FormResponseDeclined, "ada@example.com")
	if n := w.count(t, `SELECT count(*) FROM tickets WHERE form_response_id = $1 AND forms_rejected_at IS NOT NULL AND status = 'active'`, first); n != 1 {
		t.Fatalf("rejection mark %d", n)
	}
}

// "Otomatik bilet gönder" off: an accepted answer is kept as an accepted
// row and writes no Ticket; the same answer reported again is one row; a
// pending answer accepted later turns its row accepted.
func TestTheDecisionTableWithAutoTicketOff(t *testing.T) {
	w := newRespondentWorld(t, false, 0, true)
	first, second := uuid.New(), uuid.New()
	if got := w.report(t, first, ticket.FormResponseAccepted, "ada@example.com"); got.TicketsWritten != 0 || got.Outcome != formsfeed.OutcomeRespondentKept {
		t.Fatalf("accepted: %+v", got)
	}
	w.report(t, first, ticket.FormResponseAccepted, "ada@example.com")
	w.report(t, second, ticket.FormResponsePending, "grace@example.com")
	w.report(t, second, ticket.FormResponseAccepted, "grace@example.com")
	rows := w.rows(t)
	if len(rows) != 2 || rows[0].FormsStatus != formsfeed.RespondentAccepted || rows[1].FormsStatus != formsfeed.RespondentAccepted {
		t.Fatalf("rows %+v", rows)
	}
	if n := w.count(t, `SELECT count(*) FROM tickets WHERE event_id = $1`, w.ev.ID); n != 0 {
		t.Fatalf("%d Tickets written with the setting off", n)
	}
}

// Sending: a pending row is skipped and stays; an accepted one gets its
// Ticket and its row goes; the same request again writes no second Ticket.
// The preview counts by the capacity; all: true is one job, which waits
// for a running gathering and then sends the rest.
func TestSendingUnticketedRespondents(t *testing.T) {
	w := newRespondentWorld(t, false, 1, true)
	ctx := context.Background()
	w.report(t, uuid.New(), ticket.FormResponseAccepted, "ada@example.com")
	w.report(t, uuid.New(), ticket.FormResponseAccepted, "grace@example.com")
	w.report(t, uuid.New(), ticket.FormResponseAccepted, "linus@example.com")
	w.report(t, uuid.New(), ticket.FormResponsePending, "ken@example.com")
	rows := w.rows(t)
	if len(rows) != 4 {
		t.Fatalf("rows %+v", rows)
	}
	var ids []uuid.UUID
	var pending uuid.UUID
	for _, r := range rows {
		if r.FormsStatus == formsfeed.RespondentPending {
			pending = r.ID
		} else {
			ids = append(ids, r.ID)
		}
	}

	preview, err := w.rs.Preview(ctx, w.editor, w.ev.ID, formsfeed.Selection{All: true})
	if err != nil || preview != (formsfeed.Preview{Tickets: 1, Waitlisted: 2, PendingSkipped: 1}) {
		t.Fatalf("preview %+v %v", preview, err)
	}
	if _, err := w.rs.Preview(ctx, w.editor, w.ev.ID, formsfeed.Selection{}); !errors.Is(err, formsfeed.ErrRespondentsInvalid) {
		t.Fatalf("empty selection: %v", err)
	}
	if _, _, err := w.rs.Send(ctx, authz.Principal{ID: uuid.NewString()}, w.ev.ID, formsfeed.Selection{IDs: ids}); !errors.Is(err, formsfeed.ErrMappingForbidden) {
		t.Fatalf("a stranger: %v", err)
	}

	res, job, err := w.rs.Send(ctx, w.editor, w.ev.ID, formsfeed.Selection{IDs: []uuid.UUID{ids[0], pending}})
	if err != nil || job != nil || res != (formsfeed.SendResult{Recorded: 1, Skipped: 1}) {
		t.Fatalf("send %+v %v %v", res, job, err)
	}
	again, _, err := w.rs.Send(ctx, w.editor, w.ev.ID, formsfeed.Selection{IDs: []uuid.UUID{ids[0]}})
	if err != nil || again != (formsfeed.SendResult{Skipped: 1}) {
		t.Fatalf("the same request again %+v %v", again, err)
	}
	if n := w.count(t, `SELECT count(*) FROM tickets WHERE event_id = $1`, w.ev.ID); n != 1 {
		t.Fatalf("%d Tickets", n)
	}

	// all: true while a gathering of the form waits: one job, queued; it
	// waits, then sends the rest (the Event is full: the waiting list).
	if queued, err := w.pulls.Collect(ctx, w.formID, w.ev.ID, time.Now()); err != nil || !queued {
		t.Fatalf("collect %t %v", queued, err)
	}
	_, jobID, err := w.rs.Send(ctx, w.editor, w.ev.ID, formsfeed.Selection{All: true})
	if err != nil || jobID == nil {
		t.Fatalf("all %v %v", jobID, err)
	}
	_, sameID, _ := w.rs.Send(ctx, w.editor, w.ev.ID, formsfeed.Selection{All: true})
	if sameID == nil || *sameID != *jobID {
		t.Fatalf("a second all made another job %v %v", jobID, sameID)
	}
	jobs := &formsfeed.TicketJobs{Queue: w.rs.Jobs, Respondents: w.rs}
	if ran, err := jobs.RunOne(ctx); err != nil || !ran {
		t.Fatalf("run while collecting %t %v", ran, err)
	}
	if got, _ := w.rs.Job(ctx, w.editor, w.ev.ID, *jobID); got.State != formsfeed.JobQueued || got.Recorded+got.Waitlisted != 0 {
		t.Fatalf("job ran while collecting %+v", got)
	}
	if _, err := w.pool.Exec(ctx, `DELETE FROM form_pulls`); err != nil {
		t.Fatal(err)
	}
	if _, err := w.pool.Exec(ctx, `UPDATE respondent_ticket_jobs SET next_attempt_at = now()`); err != nil {
		t.Fatal(err)
	}
	if ran, err := jobs.RunOne(ctx); err != nil || !ran {
		t.Fatalf("run %t %v", ran, err)
	}
	got, err := w.rs.Job(ctx, w.editor, w.ev.ID, *jobID)
	if err != nil || got.State != formsfeed.JobDone || got.Waitlisted != 2 || got.Skipped != 1 {
		t.Fatalf("job %+v %v", got, err)
	}
	if rows := w.rows(t); len(rows) != 1 || rows[0].FormsStatus != formsfeed.RespondentPending {
		t.Fatalf("left %+v", rows)
	}
}

// A form the Event lets go takes its rows with it, in the Event's write.
func TestAFormLetGoTakesItsRows(t *testing.T) {
	w := newRespondentWorld(t, false, 0, true)
	ctx := context.Background()
	w.report(t, uuid.New(), ticket.FormResponseAccepted, "ada@example.com")
	ev, err := w.events.Get(ctx, w.ev.ID)
	if err != nil {
		t.Fatal(err)
	}
	ev.FormURL = ""
	if _, err := w.events.Update(ctx, ev); err != nil {
		t.Fatal(err)
	}
	if n := w.count(t, `SELECT count(*) FROM event_form_respondents`); n != 0 {
		t.Fatalf("%d rows stayed", n)
	}
}

// Account erasure deletes the person's rows, their account's and their
// addresses', and counts them for the proof.
func TestAccountErasureDeletesTheRespondentRows(t *testing.T) {
	w := newRespondentWorld(t, false, 0, true)
	ctx := context.Background()
	person := uuid.New()
	if _, err := w.pool.Exec(ctx, `INSERT INTO users (id, email) VALUES ($1, 'ada@example.com')`, person); err != nil {
		t.Fatal(err)
	}
	w.report(t, uuid.New(), ticket.FormResponseAccepted, "ada@example.com")
	w.report(t, uuid.New(), ticket.FormResponseAccepted, "other@example.com")
	if _, err := w.pool.Exec(ctx, `INSERT INTO event_form_respondents (id, event_id, form_id, form_response_id, user_id, forms_status, source)
		VALUES ($1, $2, $3, $4, $5, 'pending', 'collect')`, uuid.New(), w.ev.ID, w.formID, uuid.New(), person); err != nil {
		t.Fatal(err)
	}
	// The row names only an active account.
	if _, err := w.pool.Exec(ctx, `INSERT INTO event_form_respondents (id, event_id, form_id, form_response_id, user_id, forms_status, source)
		VALUES ($1, $2, $3, $4, $5, 'pending', 'collect')`, uuid.New(), w.ev.ID, w.formID, uuid.New(), uuid.New()); err == nil {
		t.Fatal("a row named an account core does not have")
	}
	jobID, err := w.rs.Jobs.Enqueue(ctx, w.ev.ID, nil, &person, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.pool.Exec(ctx, `UPDATE users SET account_state = 'deletion_pending' WHERE id = $1`, person); err != nil {
		t.Fatal(err)
	}
	counts, err := user.NewPostgresStore(w.pool).AnonymizeAccountCounted(ctx, person, time.Now(), []string{" Ada@Example.com "})
	if err != nil || counts[user.FormRespondentsErasedCount] != 2 {
		t.Fatalf("counts %v %v", counts, err)
	}
	if n := w.count(t, `SELECT count(*) FROM event_form_respondents`); n != 1 {
		t.Fatalf("%d rows left", n)
	}
	if n := w.count(t, `SELECT count(*) FROM respondent_ticket_jobs WHERE id = $1 AND requested_by IS NULL`, jobID); n != 1 {
		t.Fatal("the send-all job still names who asked")
	}
}

// "Otomatik bilet gönder" off and the person holds a Ticket: Forms' decline
// marks it (K6); the same answer accepted again clears the mark as the rule
// would; after another decline, the holder's corrected answer (Returned)
// takes the Ticket over. No row is kept for a Ticket holder.
func TestAutoTicketOffKeepsTheAcceptedAnswerOnTheTicket(t *testing.T) {
	w := newRespondentWorld(t, false, 0, true)
	ctx := context.Background()
	first, second := uuid.New(), uuid.New()
	w.report(t, first, ticket.FormResponseAccepted, "ada@example.com")
	rows := w.rows(t)
	if len(rows) != 1 {
		t.Fatalf("rows %+v", rows)
	}
	if res, _, err := w.rs.Send(ctx, w.editor, w.ev.ID, formsfeed.Selection{IDs: []uuid.UUID{rows[0].ID}}); err != nil || res.Recorded != 1 {
		t.Fatalf("send %+v %v", res, err)
	}
	state := func() string {
		t.Helper()
		var s string
		if err := w.pool.QueryRow(ctx, `SELECT COALESCE(form_response_id::text, '') || '|' || COALESCE(form_response_status, '') || '|' || (forms_rejected_at IS NOT NULL)::text
			FROM tickets WHERE event_id = $1`, w.ev.ID).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	w.report(t, first, ticket.FormResponseDeclined, "ada@example.com")
	if got := state(); got != first.String()+"|declined|true" {
		t.Fatalf("after the decline %s", got)
	}
	w.report(t, first, ticket.FormResponseAccepted, "ada@example.com")
	if got := state(); got != first.String()+"|accepted|false" {
		t.Fatalf("accepted again %s", got)
	}
	w.report(t, first, ticket.FormResponseDeclined, "ada@example.com")
	w.report(t, second, ticket.FormResponseAccepted, "ada@example.com")
	if got := state(); got != second.String()+"|accepted|false" {
		t.Fatalf("the corrected answer %s", got)
	}
	if n := len(w.rows(t)); n != 0 {
		t.Fatalf("a Ticket holder was kept (%d)", n)
	}
}

// listOf is Forms' list: the pages by cursor.
type listOf map[string]formsfeed.Page

func (l listOf) List(_ context.Context, _ uuid.UUID, q formsfeed.ListQuery) (formsfeed.Page, error) {
	if !q.AllStatuses {
		return formsfeed.Page{}, errors.New("the gathering asked for accepted answers only")
	}
	return l[q.After], nil
}

// The gathering keeps every answer of the form as a row and writes no
// Ticket: a guest's (read for the name), a signed-in person's, a mapped
// anonymous one's, a pending one's; a declined answer and a person with a
// Ticket are not kept. Run again, it keeps the same rows.
func TestTheGatheringKeepsRowsAndWritesNoTicket(t *testing.T) {
	w := newRespondentWorld(t, true, 0, true)
	ctx := context.Background()
	if _, _, err := formsfeed.NewPostgresMappings(w.pool).Put(ctx, formsfeed.Mapping{EventID: w.ev.ID, FormID: w.formID, FullName: "n", Email: "e"}); err != nil {
		t.Fatal(err)
	}
	// Ada holds a Ticket already.
	w.report(t, uuid.New(), ticket.FormResponseAccepted, "ada@example.com")
	person := uuid.New()
	if _, err := w.pool.Exec(ctx, `INSERT INTO users (id, email) VALUES ($1, 'person@example.com')`, person); err != nil {
		t.Fatal(err)
	}
	guest, ada := uuid.New(), uuid.New()
	items := []formsfeed.Response{
		{FormID: w.formID, ResponseID: guest, Status: formsfeed.StatusAccepted, HasGuest: true, Version: 1},
		{FormID: w.formID, ResponseID: uuid.New(), Status: formsfeed.StatusPending, UserID: &person, Version: 1},
		{FormID: w.formID, ResponseID: uuid.New(), Status: formsfeed.StatusAccepted, Version: 1, Answers: map[string]string{"n": "Linus Torvalds", "e": "linus@example.com"}},
		{FormID: w.formID, ResponseID: uuid.New(), Status: formsfeed.StatusDeclined, Version: 1, Answers: map[string]string{"n": "Ken Thompson", "e": "ken@example.com"}},
		{FormID: w.formID, ResponseID: ada, Status: formsfeed.StatusAccepted, Version: 1, Answers: map[string]string{"n": "Ada Lovelace", "e": "ada@example.com"}},
		{FormID: w.formID, ResponseID: uuid.New(), Status: formsfeed.StatusAccepted, Version: 1, Answers: map[string]string{"n": "Cher", "e": "cher@example.com"}},
	}
	recorder := &formsfeed.Recorder{
		Events: w.events, Record: w.record, Respondents: w.rs,
		Mapped: &formsfeed.MappedGuests{Store: formsfeed.NewPostgresMappings(w.pool), Events: w.events},
		Forms:  guestReader{guest: formsfeed.Guest{FirstName: "Grace", LastName: "Hopper", Email: "grace@example.com"}},
	}
	puller := &formsfeed.Puller{Queue: w.pulls, Forms: listOf{"": {Items: items}}, Recorder: recorder, Events: w.events, DB: w.pool}
	before := w.count(t, `SELECT count(*) FROM tickets`)
	for range 2 {
		if _, err := w.pulls.Collect(ctx, w.formID, w.ev.ID, time.Now()); err != nil {
			t.Fatal(err)
		}
		if ran, err := puller.RunOne(ctx); err != nil || !ran {
			t.Fatalf("%t %v", ran, err)
		}
	}
	if after := w.count(t, `SELECT count(*) FROM tickets`); after != before {
		t.Fatalf("the gathering wrote %d Tickets", after-before)
	}
	rows := w.rows(t)
	byStatus := map[string]int{}
	for _, r := range rows {
		if r.Source != formsfeed.SourceCollect {
			t.Fatalf("source %q", r.Source)
		}
		byStatus[r.FormsStatus]++
	}
	if len(rows) != 3 || byStatus[formsfeed.RespondentAccepted] != 2 || byStatus[formsfeed.RespondentPending] != 1 {
		t.Fatalf("rows %+v", rows)
	}
	if n := w.count(t, `SELECT count(*) FROM event_form_respondents WHERE email = 'grace@example.com' AND form_response_id = $1`, guest); n != 1 {
		t.Fatal("the guest's row lacks the name Forms read")
	}
	if n := w.count(t, `SELECT count(*) FROM form_pulls`); n != 0 {
		t.Fatalf("%d jobs left", n)
	}
	// K3: the one-word name is counted for the panel, once however often
	// the form is gathered.
	page, _, err := w.rs.List(ctx, w.editor, w.ev.ID, formsfeed.RespondentQuery{})
	if err != nil || page.Incomplete != (formsfeed.CollectCounts{MissingName: 1}) {
		t.Fatalf("incomplete %+v %v", page.Incomplete, err)
	}
}
