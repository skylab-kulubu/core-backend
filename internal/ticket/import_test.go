package ticket_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/identity"
	"github.com/skylab-kulubu/core-backend/internal/teamaudit"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// importEnv is a WEBLAB Event of the capacity given, an account holder
// (ada@example.com), a Session "Açılış" and an audit record.
type importEnv struct {
	events event.Store
	users  user.Store
	store  *ticket.MemoryStore
	svc    ticket.Service
	audit  *teamaudit.Memory
	ev     event.Event
	sess   event.Session
	adaID  uuid.UUID
}

func newImportEnv(t *testing.T, capacity int, waitlist bool) importEnv {
	t.Helper()
	ctx := context.Background()
	dir := identity.NewMemory()
	dir.PutGroup(identity.Group{ID: uuid.NewString(), Name: "WEBLAB", Path: "/UYELER/ARGE/WEBLAB"})
	e := importEnv{events: event.NewMemoryStore(), users: user.NewMemoryStore(), store: ticket.NewMemoryStore(), audit: teamaudit.NewMemory()}
	e.svc = ticket.NewService(e.store, e.events, authz.NewAuthorizer(authz.DefaultPolicy()), e.users, dir, e.audit)
	ev, err := e.events.Create(ctx, event.Event{Name: "Hack", Location: "YTÜ", OwnerTeam: "WEBLAB", Capacity: capacity, WaitlistEnabled: &waitlist})
	if err != nil {
		t.Fatal(err)
	}
	e.ev = ev
	day := seedDay(t, e.events, ev.ID, "Gün 1")
	e.sess = seedSession(t, e.events, day.ID, "Açılış")
	e.adaID = uuid.New()
	seedUser(t, e.users, e.adaID)
	return e
}

func importAs(p authz.Principal) context.Context {
	id, err := uuid.Parse(p.ID)
	if err != nil {
		id = uuid.New()
	}
	return teamaudit.WithActor(context.Background(), teamaudit.Actor{ID: id, Client: "core-frontend"})
}

var importLead = authz.Principal{ID: uuid.NewString(), Groups: []string{"/UYELER/ARGE/WEBLAB/LIDERLER"}}

const importList = "Ad,Soyad,E-posta,Oturum\n" +
	"Ada,Lovelace,ADA@example.com,Açılış\n" + // an account holder
	"Grace,Hopper,grace@example.com,Kapanış\n" + // an unknown Session
	"Grace,H.,grace@example.com,\n" + // the same e-mail again
	"Alan,,alan@example.com,\n" + // no last name
	"Linus,Torvalds,linus(at)example.com,\n" + // no address
	"Edsger,Dijkstra,edsger@example.com,\n"

func outcomes(rows []ticket.ImportRow) string {
	var b []string
	for _, r := range rows {
		s := string(r.Outcome)
		if r.Problem != "" {
			s += ":" + r.Problem
		}
		b = append(b, s)
	}
	return strings.Join(b, ",")
}

// The preview writes nothing and says, row by row, what the import would
// do; the import writes it, the members' Tickets to their accounts; the
// same list again writes nothing (docs/attendee-import.md).
func TestImportPreviewThenImportThenAgain(t *testing.T) {
	t.Parallel()
	e := newImportEnv(t, 0, true)
	in := ticket.ImportInput{Text: importList, SendMail: true}
	preview, err := e.svc.PreviewImport(importAs(importLead), importLead, e.ev.ID, in)
	if err != nil {
		t.Fatal(err)
	}
	if got := outcomes(preview.Rows); got != "active,active,duplicate,invalid:missing_last_name,invalid:invalid_email,active" {
		t.Fatalf("preview %s", got)
	}
	if preview.Imported || !preview.Rows[0].Account || preview.Rows[1].Account {
		t.Fatalf("preview %+v", preview)
	}
	if len(preview.Rows[0].Sessions) != 1 || preview.Rows[0].Sessions[0].ID != e.sess.ID || preview.Rows[1].UnknownSessions[0] != "Kapanış" {
		t.Fatalf("sessions %+v", preview.Rows[:2])
	}
	if s := preview.Summary; s.Rows != 6 || s.Active != 3 || s.Members != 1 || s.Guests != 2 || s.Duplicates != 1 || s.Invalid != 2 || s.UnknownSessions != 1 {
		t.Fatalf("summary %+v", s)
	}
	if listed, _ := e.svc.ListByEvent(context.Background(), yk, e.ev.ID); len(listed) != 0 {
		t.Fatalf("the preview wrote %d tickets", len(listed))
	}
	if rows := e.audit.Rows(); len(rows) != 1 || rows[0].Action != teamaudit.ActionTicketImportPreview ||
		rows[0].After.Import == nil || rows[0].After.Import.Created != 3 || rows[0].After.Import.Members != 1 || rows[0].After.Import.Guests != 2 {
		t.Fatalf("the preview's audit row: %+v", rows)
	}

	done, err := e.svc.ImportAttendees(importAs(importLead), importLead, e.ev.ID, in)
	if err != nil {
		t.Fatal(err)
	}
	if !done.Imported || done.Summary.Active != 3 {
		t.Fatalf("import %+v", done.Summary)
	}
	listed, err := e.svc.ListByEvent(context.Background(), yk, e.ev.ID)
	if err != nil || len(listed) != 3 {
		t.Fatalf("%d tickets %v", len(listed), err)
	}
	members := 0
	for _, tk := range listed {
		if tk.OwnerID != nil && *tk.OwnerID == e.adaID {
			members++
		}
		if tk.TicketType == ticket.Guest && tk.GuestEmail == "grace@example.com" && (tk.GuestFirstName != "Grace" || tk.GuestLastName != "Hopper") {
			t.Fatalf("the first row of an e-mail wins: %+v", tk)
		}
	}
	if members != 1 {
		t.Fatal("the account holder's Ticket is not theirs")
	}

	again, err := e.svc.ImportAttendees(importAs(importLead), importLead, e.ev.ID, in)
	if err != nil {
		t.Fatal(err)
	}
	if again.Summary.Active != 0 || again.Summary.Existing != 3 {
		t.Fatalf("again %+v", again.Summary)
	}
	if listed, _ := e.svc.ListByEvent(context.Background(), yk, e.ev.ID); len(listed) != 3 {
		t.Fatalf("the second import wrote: %d tickets", len(listed))
	}

	rows := e.audit.Rows()
	if len(rows) != 3 {
		t.Fatalf("%d audit rows", len(rows))
	}
	first := rows[1]
	if first.Action != teamaudit.ActionTicketImport || first.Outcome != teamaudit.OutcomeOK || first.After == nil ||
		first.After.EventID != e.ev.ID.String() || first.After.TicketID != "" || first.After.Import == nil ||
		first.After.Import.Created != 3 || first.After.Import.Members != 1 || !first.After.Import.Mail ||
		first.Actor.Basis != teamaudit.BasisLeader || first.After.Import.Source != "" || first.After.Import.JobID != "" {
		t.Fatalf("audit row %+v %+v", first, first.After)
	}
}

// A form respondents' sending's row says so (source form_respondents); a
// send-all job's row stays pending until the job ended, then holds the
// job's id and its own counts (docs/form-respondents.md).
func TestFormRespondentSendAuditRows(t *testing.T) {
	t.Parallel()
	e := newImportEnv(t, 0, true)
	ctx := importAs(importLead)

	now, err := e.svc.BeginFormRespondentSend(ctx, importLead, e.ev.ID)
	if err != nil {
		t.Fatal(err)
	}
	now.Finish(ctx, ticket.FormRespondentSummary{Rows: 2, Recorded: 1, Existing: 1, Members: 1, Guests: 1}, nil)

	job, err := e.svc.BeginFormRespondentSend(ctx, importLead, e.ev.ID)
	if err != nil {
		t.Fatal(err)
	}
	if job.ID() == uuid.Nil {
		t.Fatal("no row id for the job")
	}
	rows := e.audit.Rows()
	if len(rows) != 2 || rows[1].Outcome != teamaudit.OutcomePending {
		t.Fatalf("the job's row before its end: %+v", rows)
	}
	e.svc.FinishFormRespondentJob(context.Background(), e.ev.ID, job.ID(), ticket.FormRespondentSummary{Rows: 7, Recorded: 3, Waitlisted: 2, Full: 1, Skipped: 1}, nil)
	// A job with no pending row (the record was off, or another request
	// queued it) writes nothing.
	e.svc.FinishFormRespondentJob(context.Background(), e.ev.ID, uuid.New(), ticket.FormRespondentSummary{}, nil)

	rows = e.audit.Rows()
	if len(rows) != 2 {
		t.Fatalf("%d audit rows", len(rows))
	}
	sent, done := rows[0], rows[1]
	if sent.Action != teamaudit.ActionTicketImport || sent.Outcome != teamaudit.OutcomeOK || sent.After.Import == nil ||
		sent.After.Import.Source != teamaudit.SourceFormRespondents || sent.After.Import.JobID != "" || sent.After.Import.Created != 1 {
		t.Fatalf("sending row %+v %+v", sent, sent.After)
	}
	if done.Outcome != teamaudit.OutcomeOK || done.After == nil || done.After.EventID != e.ev.ID.String() || done.After.Import == nil ||
		done.After.Import.Source != teamaudit.SourceFormRespondents || done.After.Import.JobID != job.ID().String() ||
		done.After.Import.Rows != 7 || done.After.Import.Created != 3 || done.After.Import.Waitlisted != 2 ||
		done.After.Import.Full != 1 || done.After.Import.Skipped != 1 {
		t.Fatalf("job row %+v %+v", done, done.After)
	}
}

// Under the capacity the rows fill the places left, then the waiting list;
// with the waiting list off they are full; past the capacity every one is
// active and says so.
func TestImportCapacity(t *testing.T) {
	t.Parallel()
	list := "ad,soyad,eposta\nA,A,a@example.com\nB,B,b@example.com\nC,C,c@example.com\n"
	for _, tc := range []struct {
		name     string
		waitlist bool
		past     bool
		want     string
	}{
		{"waitlist on", true, false, "active,active,waitlisted"},
		{"waitlist off", false, false, "active,active,full"},
		{"past the capacity", false, true, "active,active,active"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := newImportEnv(t, 2, tc.waitlist)
			in := ticket.ImportInput{Text: list, SendMail: true, PastCapacity: tc.past}
			preview, err := e.svc.PreviewImport(importAs(importLead), importLead, e.ev.ID, in)
			if err != nil {
				t.Fatal(err)
			}
			if got := outcomes(preview.Rows); got != tc.want {
				t.Fatalf("preview %s", got)
			}
			if preview.Capacity.Capacity != 2 || preview.Capacity.Free != 2 || preview.Capacity.WaitlistEnabled != tc.waitlist {
				t.Fatalf("capacity %+v", preview.Capacity)
			}
			done, err := e.svc.ImportAttendees(importAs(importLead), importLead, e.ev.ID, in)
			if err != nil {
				t.Fatal(err)
			}
			if got := outcomes(done.Rows); got != tc.want {
				t.Fatalf("import %s", got)
			}
			if tc.past && (!done.Rows[2].OverCapacity || done.Summary.OverCapacity != 1) {
				t.Fatalf("over %+v", done.Rows[2])
			}
		})
	}
}

// Only an operator of the Event imports: another team's member and a
// service account get 403, a draft they may not see 404; a cancelled Event
// takes nobody.
func TestImportRights(t *testing.T) {
	t.Parallel()
	e := newImportEnv(t, 0, true)
	in := ticket.ImportInput{Text: "ad,soyad,eposta\nA,B,a@example.com\n"}
	for name, p := range map[string]authz.Principal{"another team's member": otherMember, "forms service": formsHop, "anonymous": anonymous} {
		if _, err := e.svc.PreviewImport(importAs(p), p, e.ev.ID, in); !errors.Is(err, ticket.ErrForbidden) {
			t.Errorf("%s preview: %v", name, err)
		}
		if _, err := e.svc.ImportAttendees(importAs(p), p, e.ev.ID, in); !errors.Is(err, ticket.ErrForbidden) {
			t.Errorf("%s import: %v", name, err)
		}
	}
	if _, err := e.svc.PreviewImport(importAs(yk), yk, uuid.New(), in); !errors.Is(err, ticket.ErrNotFound) {
		t.Fatalf("unknown event: %v", err)
	}
	draft, err := e.events.Create(context.Background(), event.Event{Name: "Taslak", Location: "YTÜ", OwnerTeam: "WEBLAB", Status: event.StatusDraft})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.PreviewImport(importAs(otherMember), otherMember, draft.ID, in); !errors.Is(err, ticket.ErrNotFound) {
		t.Fatalf("a draft to who may not see it: %v", err)
	}
	// Its operator imports into the draft (speakers, the team), as
	// "Katılımcı ekle".
	report, err := e.svc.PreviewImport(importAs(importLead), importLead, draft.ID, in)
	if err != nil || report.Summary.Active != 1 || report.Mail != ticket.ImportMailOff {
		t.Fatalf("a draft: %+v %v", report, err)
	}
	if _, err := e.events.Cancel(context.Background(), e.ev.ID, nil, "", false); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.PreviewImport(importAs(importLead), importLead, e.ev.ID, in); !errors.Is(err, ticket.ErrEventCancelled) {
		t.Fatalf("a cancelled event: %v", err)
	}
}

// An audit row that cannot be opened stops the import before it writes.
func TestImportStopsWithoutAuditRow(t *testing.T) {
	t.Parallel()
	e := newImportEnv(t, 0, true)
	e.audit.SetUnavailable(true)
	_, err := e.svc.ImportAttendees(importAs(importLead), importLead, e.ev.ID, ticket.ImportInput{Text: "ad,soyad,eposta\nA,B,a@example.com\n"})
	if !errors.Is(err, teamaudit.ErrUnavailable) {
		t.Fatalf("%v", err)
	}
	if listed, _ := e.svc.ListByEvent(context.Background(), yk, e.ev.ID); len(listed) != 0 {
		t.Fatal("written without an audit row")
	}
}

// A person who registered on their own, or whose Ticket was cancelled, is
// left as they are.
func TestImportLeavesExistingTickets(t *testing.T) {
	t.Parallel()
	e := newImportEnv(t, 0, true)
	ctx := context.Background()
	applied, err := e.svc.ApplyGuest(ctx, importLead, e.ev.ID, ticket.GuestInfo{FirstName: "Grace", LastName: "Hopper", Email: "grace@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.CancelTicket(importAs(importLead), importLead, applied.Ticket.ID, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Apply(ctx, authz.Principal{ID: e.adaID.String()}, e.ev.ID); err != nil {
		t.Fatal(err)
	}
	report, err := e.svc.ImportAttendees(importAs(importLead), importLead, e.ev.ID, ticket.ImportInput{
		Text: "ad,soyad,eposta\nAda,L,ada@example.com\nGrace,H,GRACE@example.com\n",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := outcomes(report.Rows); got != "existing,existing" {
		t.Fatalf("%s", got)
	}
	if report.Rows[0].TicketStatus != ticket.StatusActive || report.Rows[1].TicketStatus != ticket.StatusCancelled {
		t.Fatalf("%+v", report.Rows)
	}
}

// A row with a NUL, a control character or a byte that was not UTF-8 (a
// Windows-1254 CSV) is invalid; the others are imported.
func TestImportBadCharactersRow(t *testing.T) {
	t.Parallel()
	e := newImportEnv(t, 0, true)
	list := "ad,soyad,eposta\nAda,Lovelace,ada@example.com\nNul\x00l,X,nul@example.com\nAy\xfee,Y\xfdlmaz,ayse@example.com\nBell\a,Z,bell@example.com\n"
	report, err := e.svc.ImportAttendees(importAs(importLead), importLead, e.ev.ID, ticket.ImportInput{Text: list})
	if err != nil {
		t.Fatal(err)
	}
	if got := outcomes(report.Rows); got != "active,invalid:invalid_characters,invalid:invalid_characters,invalid:invalid_characters" {
		t.Fatalf("%s", got)
	}
}

// failingStore fails the second Ticket it is asked to write, as a database
// that went away midway.
type failingStore struct {
	*ticket.MemoryStore
	calls int
}

func (f *failingStore) CreateAdmitted(ctx context.Context, t ticket.Ticket, a ticket.Admission) (ticket.Ticket, error) {
	f.calls++
	if f.calls == 2 {
		return ticket.Ticket{}, errors.New("connection lost")
	}
	return f.MemoryStore.CreateAdmitted(ctx, t, a)
}

// An import that fails midway counts in its audit row only what it wrote.
func TestImportFailureCountsWrittenRowsOnly(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := identity.NewMemory()
	dir.PutGroup(identity.Group{ID: uuid.NewString(), Name: "WEBLAB", Path: "/UYELER/ARGE/WEBLAB"})
	events := event.NewMemoryStore()
	mem := ticket.NewMemoryStore()
	mem.UseEvents(events)
	store := &failingStore{MemoryStore: mem}
	audit := teamaudit.NewMemory()
	svc := ticket.NewService(store, events, authz.NewAuthorizer(authz.DefaultPolicy()), user.NewMemoryStore(), dir, audit)
	ev, err := events.Create(ctx, event.Event{Name: "Hack", Location: "YTÜ", OwnerTeam: "WEBLAB"})
	if err != nil {
		t.Fatal(err)
	}
	list := "ad,soyad,eposta\nA,A,a@example.com\nB,B,b@example.com\nC,C,c@example.com\n"
	if _, err := svc.ImportAttendees(importAs(importLead), importLead, ev.ID, ticket.ImportInput{Text: list}); err == nil {
		t.Fatal("the failure was not answered")
	}
	rows := audit.Rows()
	if len(rows) != 1 || rows[0].Outcome != teamaudit.OutcomeFailed || rows[0].After.Import == nil ||
		rows[0].After.Import.Created != 1 || rows[0].After.Import.Rows != 3 {
		t.Fatalf("audit %+v %+v", rows, rows[0].After.Import)
	}
}
