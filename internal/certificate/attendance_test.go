package certificate_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/certificate"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// attendanceFixture is a count Event with marked Sessions:
//
//	Açılış  counted, required
//	Talk 1  counted
//	Talk 2  counted
//	Talk 3  counted
//	Ağ      not counted (NETWORKING)
//	Final   counted, required, cancelled: out of N and of the required ones
//
// N = 4, the default threshold ⌈3·4/4⌉ = 3.
type attendanceFixture struct {
	events   *event.MemoryStore
	tickets  *ticket.MemoryStore
	users    *user.MemoryStore
	store    *certificate.MemoryStore
	svc      certificate.Service
	ev       event.Event
	sessions map[string]event.Session
	leader   authz.Principal
}

func newAttendanceFixture(t *testing.T, rule string) attendanceFixture {
	t.Helper()
	ctx := context.Background()
	f := attendanceFixture{
		events: event.NewMemoryStore(), tickets: ticket.NewMemoryStore(), users: user.NewMemoryStore(),
		store: certificate.NewMemoryStore(), sessions: map[string]event.Session{},
		leader: authz.Principal{ID: uuid.NewString(), Groups: []string{"/UYELER/ARGE/ARTLAB/LIDERLER"}},
	}
	f.svc = certificate.NewServiceWithOptions(f.store, f.tickets, f.events, f.users, authz.NewAuthorizer(authz.DefaultPolicy()), &recRender{}, &recMail{}, certificate.Options{
		PublicAPIOrigin: "https://api.example.test", VerifyOrigin: "https://skyl.app/c", Templates: f.store, Jobs: f.store,
	})
	ev, err := f.events.Create(ctx, event.Event{Name: "ARTLAB 2026", Location: "YTÜ", OwnerTeam: "ARTLAB", AttendanceRule: rule})
	if err != nil {
		t.Fatal(err)
	}
	f.ev = ev
	day, err := f.events.CreateDay(ctx, event.Day{EventID: ev.ID, Name: "Gün 1"})
	if err != nil {
		t.Fatal(err)
	}
	for i, s := range []struct {
		title                        string
		counted, required, cancelled bool
	}{
		{"Açılış", true, true, false},
		{"Talk 1", true, false, false},
		{"Talk 2", true, false, false},
		{"Talk 3", true, false, false},
		{"Ağ", false, false, false},
		{"Final", true, true, true},
	} {
		sess, err := f.events.CreateSession(ctx, event.Session{
			EventDayID: day.ID, Title: s.title, SpeakerName: "Ada", SessionType: "PRESENTATION", OrderIndex: i,
			CertificateCounted: s.counted, CertificateRequired: s.required, Cancelled: s.cancelled,
		})
		if err != nil {
			t.Fatal(err)
		}
		f.sessions[s.title] = sess
	}
	template, err := f.store.CreateTemplate(ctx, certificate.Template{ID: uuid.New(), Name: "ARTLAB", OwnerTeam: "ARTLAB", SourceKind: "figma", DraftLayout: versionedLayout("V1")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.CreateVersion(ctx, certificate.TemplateVersion{ID: uuid.New(), TemplateID: template.ID, Layout: template.DraftLayout, Checksum: "one"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.SetBinding(ctx, certificate.Binding{ID: uuid.New(), Scope: certificate.ScopeOwnerTeam, ScopeKey: "ARTLAB", TemplateID: template.ID}); err != nil {
		t.Fatal(err)
	}
	return f
}

// member adds a member's Ticket checked in on the named Sessions.
func (f attendanceFixture) member(t *testing.T, first, last string, sessions ...string) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	ownerID := uuid.New()
	if _, _, err := f.users.Upsert(ctx, user.User{ID: ownerID, Email: strings.ToLower(first) + "@example.com", FirstName: first, LastName: last}); err != nil {
		t.Fatal(err)
	}
	return f.ticket(t, ticket.Ticket{EventID: f.ev.ID, TicketType: ticket.Registered, OwnerID: &ownerID}, sessions...)
}

// guest adds a Guest's Ticket checked in on the named Sessions.
func (f attendanceFixture) guest(t *testing.T, first, last string, sessions ...string) uuid.UUID {
	t.Helper()
	return f.ticket(t, ticket.Ticket{EventID: f.ev.ID, TicketType: ticket.Guest, GuestFirstName: first, GuestLastName: last, GuestEmail: strings.ToLower(first) + "@example.com"}, sessions...)
}

func (f attendanceFixture) ticket(t *testing.T, item ticket.Ticket, sessions ...string) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	item.ID = uuid.New()
	created, err := f.tickets.Create(ctx, item)
	if err != nil {
		t.Fatal(err)
	}
	for _, title := range sessions {
		sess, ok := f.sessions[title]
		if !ok {
			t.Fatalf("no session %q", title)
		}
		if _, err := f.tickets.AddCheckIn(ctx, ticket.CheckIn{ID: uuid.New(), TicketID: created.ID, EventDayID: sess.EventDayID, SessionID: sess.ID}); err != nil {
			t.Fatal(err)
		}
	}
	return created.ID
}

func rowsByTicket(list certificate.AttendanceList) map[uuid.UUID]certificate.AttendanceRow {
	out := map[uuid.UUID]certificate.AttendanceRow{}
	for _, row := range list.Rows {
		out[row.TicketID] = row
	}
	return out
}

// The list counts check-ins on counted live Sessions only, misses only live
// required Sessions (a cancelled required one drops out), names who the
// certificate would name, orders by name and carries no e-mail.
func TestAttendanceListCountsTheMarkedSessions(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newAttendanceFixture(t, certificate.RuleCount)
	ayse := f.member(t, "Ayşe", "Yılmaz", "Açılış", "Talk 1", "Talk 2")
	burak := f.member(t, "Burak", "Kaya", "Talk 1", "Talk 2", "Talk 3", "Ağ")
	cem := f.member(t, "Cem", "Demir", "Açılış", "Ağ")
	deniz := f.guest(t, "Deniz", "Ak", "Açılış", "Talk 1", "Talk 2", "Talk 3")

	list, err := f.svc.Attendance(ctx, f.leader, f.ev.ID)
	if err != nil {
		t.Fatal(err)
	}
	if list.CountedTotal != 4 || list.EffectiveMin != 3 || list.Rule != certificate.RuleCount {
		t.Fatalf("header %+v", list)
	}
	if len(list.Required) != 1 || list.Required[0].Title != "Açılış" {
		t.Fatalf("required %+v", list.Required)
	}
	rows := rowsByTicket(list)
	for _, want := range []struct {
		id       uuid.UUID
		name     string
		attended int
		missing  []string
		status   string
	}{
		{ayse, "Ayşe Yılmaz", 3, nil, certificate.StatusEligible},
		// The NETWORKING check-in is not counted; Açılış is missing.
		{burak, "Burak Kaya", 3, []string{"Açılış"}, certificate.StatusBelow},
		{cem, "Cem Demir", 1, nil, certificate.StatusBelow},
		{deniz, "Deniz Ak", 4, nil, certificate.StatusEligible},
	} {
		row := rows[want.id]
		titles := []string{}
		for _, m := range row.MissingRequired {
			titles = append(titles, m.Title)
		}
		if row.Name != want.name || row.Attended != want.attended || row.Status != want.status || strings.Join(titles, ",") != strings.Join(want.missing, ",") {
			t.Errorf("%s: %+v, want attended=%d missing=%v status=%s", want.name, row, want.attended, want.missing, want.status)
		}
		if row.Certificate != nil {
			t.Errorf("%s has a certificate %+v", want.name, row.Certificate)
		}
	}
	if rows[deniz].TicketType != ticket.Guest {
		t.Fatalf("guest type %q", rows[deniz].TicketType)
	}
	names := []string{}
	for _, row := range list.Rows {
		names = append(names, row.Name)
	}
	if strings.Join(names, "|") != "Ayşe Yılmaz|Burak Kaya|Cem Demir|Deniz Ak" {
		t.Fatalf("order %v", names)
	}
	raw, _ := json.Marshal(list)
	if strings.Contains(string(raw), "@") || strings.Contains(strings.ToLower(string(raw)), "email") {
		t.Fatalf("the list carries an e-mail: %s", raw)
	}

	summary, err := f.svc.Summary(ctx, f.leader, f.ev.ID)
	if err != nil {
		t.Fatal(err)
	}
	if summary.CountedTotal != 4 || summary.EffectiveMin != 3 || summary.EligibleCount != 2 || summary.BelowCount != 2 {
		t.Fatalf("summary %+v", summary)
	}

	// A member of the team reads neither.
	member := authz.Principal{ID: uuid.NewString(), Groups: []string{"/UYELER/ARGE/ARTLAB"}}
	if _, err := f.svc.Attendance(ctx, member, f.ev.ID); !errors.Is(err, certificate.ErrForbidden) {
		t.Fatalf("member reads the list: %v", err)
	}
}

// A set threshold above N applies as N; a required Session is needed even
// when the threshold is met.
func TestAttendanceListThresholdIsMinOfMAndN(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newAttendanceFixture(t, certificate.RuleCount)
	six := 6
	f.ev.AttendanceMinCount = &six
	if _, err := f.events.Update(ctx, f.ev); err != nil {
		t.Fatal(err)
	}
	all := f.member(t, "Ayşe", "Yılmaz", "Açılış", "Talk 1", "Talk 2", "Talk 3")
	three := f.member(t, "Burak", "Kaya", "Açılış", "Talk 1", "Talk 2")
	list, err := f.svc.Attendance(ctx, f.leader, f.ev.ID)
	if err != nil {
		t.Fatal(err)
	}
	rows := rowsByTicket(list)
	if list.EffectiveMin != 4 || rows[all].Status != certificate.StatusEligible || rows[three].Status != certificate.StatusBelow {
		t.Fatalf("min(6, 4): %+v", list)
	}
}

// Status precedence: issued > queued > failed > revoked > eligible > below.
func TestAttendanceStatusPrecedence(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newAttendanceFixture(t, certificate.RuleCount)
	issued := f.member(t, "Ayşe", "Yılmaz", "Açılış", "Talk 1", "Talk 2")
	revokedThenIssued := f.member(t, "Burak", "Kaya", "Açılış", "Talk 1", "Talk 2")
	failed := f.member(t, "Cem", "Demir")
	revoked := f.member(t, "Deniz", "Ak", "Açılış", "Talk 1", "Talk 2")
	queued := f.member(t, "Ece", "Su")
	eligible := f.member(t, "Fatma", "Er", "Açılış", "Talk 1", "Talk 2")
	below := f.member(t, "Gül", "Ay", "Talk 1")

	cert := func(ticketID uuid.UUID) certificate.Certificate {
		t.Helper()
		c, err := f.store.Create(ctx, certificate.Certificate{EventID: f.ev.ID, TicketID: ticketID, Serial: strings.ToUpper(strings.ReplaceAll(uuid.NewString(), "-", "")), RecipientName: "x", RecipientEmail: "x@example.com", EventName: f.ev.Name, OwnerTeam: "ARTLAB"}, nil)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	revoke := func(c certificate.Certificate) {
		t.Helper()
		if _, err := f.store.Revoke(ctx, c.Serial); err != nil {
			t.Fatal(err)
		}
	}
	cert(issued)
	revoke(cert(revokedThenIssued))
	active := cert(revokedThenIssued)
	revoke(cert(revoked))

	// A failed job: queued, claimed and failed for good.
	if _, err := f.svc.QueueManual(ctx, f.leader, f.ev.ID, failed, "Raporlu"); err != nil {
		t.Fatal(err)
	}
	claimed, err := f.store.ClaimJobs(ctx, 10, time.Minute)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim %v %+v", err, claimed)
	}
	if err := f.store.FailJob(ctx, claimed[0].ID, claimed[0].LeaseToken, "render", time.Now(), true); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.QueueManual(ctx, f.leader, f.ev.ID, queued, "Raporlu"); err != nil {
		t.Fatal(err)
	}

	list, err := f.svc.Attendance(ctx, f.leader, f.ev.ID)
	if err != nil {
		t.Fatal(err)
	}
	rows := rowsByTicket(list)
	for id, want := range map[uuid.UUID]string{
		issued: certificate.StatusIssued, revokedThenIssued: certificate.StatusIssued, failed: certificate.StatusFailed,
		revoked: certificate.StatusRevoked, queued: certificate.StatusQueued, eligible: certificate.StatusEligible, below: certificate.StatusBelow,
	} {
		if rows[id].Status != want {
			t.Errorf("%s: %s, want %s", rows[id].Name, rows[id].Status, want)
		}
	}
	if c := rows[revokedThenIssued].Certificate; c == nil || c.Serial != active.Serial || c.RevokedAt != nil {
		t.Fatalf("issued after a revocation shows %+v, want the active %s", c, active.Serial)
	}
	if c := rows[revoked].Certificate; c == nil || c.RevokedAt == nil {
		t.Fatalf("revoked shows %+v", c)
	}
	summary, err := f.svc.Summary(ctx, f.leader, f.ev.ID)
	if err != nil {
		t.Fatal(err)
	}
	if summary.BelowCount != 1 {
		t.Fatalf("below %d, want only the one with nothing", summary.BelowCount)
	}
}

// Attendance finalization queues the eligible Tickets by the marks, and
// none that already has a certificate.
func TestFinalizeQueuesByTheCountedAndRequiredSessions(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newAttendanceFixture(t, certificate.RuleCount)
	eligible := f.member(t, "Ayşe", "Yılmaz", "Açılış", "Talk 1", "Talk 2")
	f.member(t, "Burak", "Kaya", "Talk 1", "Talk 2", "Talk 3", "Ağ")
	f.member(t, "Cem", "Demir", "Açılış", "Talk 1", "Ağ")
	already := f.member(t, "Deniz", "Ak", "Açılış", "Talk 1", "Talk 2", "Talk 3")
	if _, err := f.store.Create(ctx, certificate.Certificate{EventID: f.ev.ID, TicketID: already, Serial: "DENIZ", RecipientName: "Deniz Ak", RecipientEmail: "deniz@example.com", EventName: f.ev.Name, OwnerTeam: "ARTLAB"}, nil); err != nil {
		t.Fatal(err)
	}
	batch, err := f.svc.Finalize(ctx, f.leader, f.ev.ID)
	if err != nil {
		t.Fatal(err)
	}
	if batch.TotalCount != 1 || len(batch.Jobs) != 1 || batch.Jobs[0].TicketID != eligible {
		t.Fatalf("finalization %+v", batch)
	}
}

// A manual issue below the rule needs a justification (trimmed, at most 500
// characters), which the batch keeps; an eligible Ticket needs none.
func TestQueueManualJustification(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newAttendanceFixture(t, certificate.RuleCount)
	below := f.member(t, "Burak", "Kaya", "Talk 1")
	eligible := f.member(t, "Ayşe", "Yılmaz", "Açılış", "Talk 1", "Talk 2")

	for _, blank := range []string{"", "   \n"} {
		if _, err := f.svc.QueueManual(ctx, f.leader, f.ev.ID, below, blank); !errors.Is(err, certificate.ErrJustificationRequired) {
			t.Fatalf("below with %q: %v", blank, err)
		}
	}
	if _, err := f.svc.QueueManual(ctx, f.leader, f.ev.ID, below, strings.Repeat("ş", 501)); !errors.Is(err, certificate.ErrJustificationTooLong) {
		t.Fatalf("501 characters: %v", err)
	}
	batch, err := f.svc.QueueManual(ctx, f.leader, f.ev.ID, below, "  Raporlu, iki oturumu kaçırdı ")
	if err != nil {
		t.Fatal(err)
	}
	if batch.Reason != "manual" || batch.Justification == nil || *batch.Justification != "Raporlu, iki oturumu kaçırdı" ||
		batch.RequestedBy == nil || batch.RequestedBy.String() != f.leader.ID {
		t.Fatalf("batch %+v", batch)
	}
	listed, err := f.svc.ListBatches(ctx, f.leader, f.ev.ID)
	if err != nil || len(listed) != 1 || listed[0].Justification == nil || *listed[0].Justification != "Raporlu, iki oturumu kaçırdı" {
		t.Fatalf("listed %v %+v", err, listed)
	}
	got, err := f.svc.GetBatch(ctx, f.leader, batch.ID)
	if err != nil || got.Justification == nil {
		t.Fatalf("get %v %+v", err, got)
	}

	plain, err := f.svc.QueueManual(ctx, f.leader, f.ev.ID, eligible, "")
	if err != nil || plain.Justification != nil {
		t.Fatalf("eligible without a justification: %v %+v", err, plain)
	}
	long := f.member(t, "Cem", "Demir")
	if _, err := f.svc.QueueManual(ctx, f.leader, f.ev.ID, long, strings.Repeat("ş", 500)); err != nil {
		t.Fatalf("500 characters: %v", err)
	}
}

// The fixture trap guard: a Session written through the store with no marks
// (Session{}) is not counted, as the Postgres store writes the mark it is
// given. An Event with no counted Session has N = 0: no Ticket is eligible
// whatever it attended, the threshold is 0, every Ticket is below, and a
// manual issue needs a justification.
func TestUncountedFixtureIssuesNothing(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for _, rule := range []string{certificate.RuleOnce, certificate.RuleCount} {
		f := newAttendanceFixture(t, rule)
		day, err := f.events.CreateDay(ctx, event.Day{EventID: f.ev.ID, Name: "Gün 2"})
		if err != nil {
			t.Fatal(err)
		}
		// Unmark the fixture's Sessions and add one with no marks.
		for _, sess := range f.sessions {
			sess.CertificateCounted, sess.CertificateRequired = false, false
			if _, err := f.events.UpdateSession(ctx, sess); err != nil {
				t.Fatal(err)
			}
		}
		bare, err := f.events.CreateSession(ctx, event.Session{EventDayID: day.ID, Title: "Bare", SpeakerName: "Ada", SessionType: "PRESENTATION"})
		if err != nil {
			t.Fatal(err)
		}
		if bare.CertificateCounted {
			t.Fatal("a Session{} is counted: the fixtures no longer need CertificateCounted")
		}
		f.sessions["Bare"] = bare
		id := f.member(t, "Ayşe", "Yılmaz", "Açılış", "Talk 1", "Talk 2", "Talk 3", "Bare")
		list, err := f.svc.Attendance(ctx, f.leader, f.ev.ID)
		if err != nil {
			t.Fatal(err)
		}
		if list.CountedTotal != 0 || list.EffectiveMin != 0 || len(list.Rows) != 1 || list.Rows[0].Attended != 0 || list.Rows[0].Status != certificate.StatusBelow {
			t.Fatalf("%s with N = 0: %+v", rule, list)
		}
		summary, err := f.svc.Summary(ctx, f.leader, f.ev.ID)
		if err != nil || summary.EligibleCount != 0 || summary.BelowCount != 1 {
			t.Fatalf("%s summary %v %+v", rule, err, summary)
		}
		if _, err := f.svc.QueueManual(ctx, f.leader, f.ev.ID, id, ""); !errors.Is(err, certificate.ErrJustificationRequired) {
			t.Fatalf("%s manual issue with N = 0: %v", rule, err)
		}
	}
}
