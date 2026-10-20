package eventstatusmail_test

import (
	"context"
	"io"
	"log"
	"slices"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/eventstatusmail"
	"github.com/skylab-kulubu/core-backend/internal/mail"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

type sent struct {
	kind mail.EventStatusKind
	to   string
	name string
	vars map[string]string
}

// fakeMail records each send; fail answers an error for an address.
type fakeMail struct {
	mu   sync.Mutex
	off  map[mail.EventStatusKind]bool
	fail map[string]error
	sent []sent
}

func (m *fakeMail) EventStatusConfigured(kind mail.EventStatusKind) bool { return !m.off[kind] }

func (m *fakeMail) EventStatus(_ context.Context, kind mail.EventStatusKind, to, name string, vars map[string]string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.fail[to]; err != nil {
		return err
	}
	m.sent = append(m.sent, sent{kind, to, name, vars})
	return nil
}

func (m *fakeMail) to(kind mail.EventStatusKind) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for _, s := range m.sent {
		if s.kind == kind {
			out = append(out, s.to)
		}
	}
	sort.Strings(out)
	return out
}

type env struct {
	events  *event.MemoryStore
	tickets *ticket.MemoryStore
	users   *user.MemoryStore
	svc     event.Service
	mail    *fakeMail
	worker  *eventstatusmail.Service
	shift   time.Duration
	ev      event.Event
	leader  authz.Principal
}

func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{
		events: event.NewMemoryStore(), tickets: ticket.NewMemoryStore(), users: user.NewMemoryStore(),
		mail:   &fakeMail{off: map[mail.EventStatusKind]bool{}, fail: map[string]error{}},
		leader: authz.Principal{ID: uuid.NewString(), Groups: []string{"/UYELER/ARGE/WEBLAB/LIDERLER"}},
	}
	e.svc = event.NewService(e.events, authz.NewAuthorizer(authz.DefaultPolicy()))
	worker, err := eventstatusmail.NewService(eventstatusmail.Config{
		Queue: e.events, Events: e.events, Tickets: e.tickets, Users: e.users, Mail: e.mail,
		Now: func() time.Time { return time.Now().UTC().Add(e.shift) }, Logger: log.New(io.Discard, "", 0),
	})
	if err != nil {
		t.Fatal(err)
	}
	e.worker = worker
	start := time.Date(2030, 3, 14, 16, 0, 0, 0, time.UTC)
	if e.ev, err = e.events.Create(t.Context(), event.Event{Name: "Gecekodu", Location: "YTÜ", OwnerTeam: "WEBLAB", StartDate: &start}); err != nil {
		t.Fatal(err)
	}
	return e
}

// member writes a registered Ticket for a person with an account.
func (e *env) member(t *testing.T, email, first string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, _, err := user.NewService(e.users).Ensure(t.Context(), id, user.Profile{Email: email, FirstName: first, LastName: "Soyad"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.tickets.Create(t.Context(), ticket.Ticket{EventID: e.ev.ID, TicketType: ticket.Registered, OwnerID: &id}); err != nil {
		t.Fatal(err)
	}
	return id
}

func (e *env) guest(t *testing.T, email, first string) {
	t.Helper()
	if _, err := e.tickets.Create(t.Context(), ticket.Ticket{EventID: e.ev.ID, TicketType: ticket.Guest, GuestEmail: email, GuestFirstName: first, GuestLastName: "Misafir"}); err != nil {
		t.Fatal(err)
	}
}

func (e *env) pass(t *testing.T) eventstatusmail.Report {
	t.Helper()
	report, err := e.worker.Pass(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return report
}

func TestCancellationMailReachesEachHolderOnce(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.member(t, "ada@example.com", "Ada")
	e.guest(t, "grace@example.com", "Grace")
	// The same address on a registered and a guest Ticket: one mail.
	e.member(t, "ikiz@example.com", "İkiz")
	e.guest(t, "IKIZ@example.com", "İkiz")
	// Erasure requested: no mail.
	gone := e.member(t, "gone@example.com", "Gone")
	if _, err := e.users.RequestDeletion(t.Context(), gone, nil); err != nil {
		t.Fatal(err)
	}
	// A guest without an address: none either.
	e.guest(t, "", "Adsız")

	cancelled, err := e.svc.Cancel(t.Context(), e.leader, e.ev.ID, "Hava koşulları", true)
	if err != nil {
		t.Fatal(err)
	}
	if cancelled.CancelNotifiedAt == nil {
		t.Fatalf("cancelNotifiedAt %+v", cancelled)
	}
	// A second cancel queues nothing again.
	if _, err := e.svc.Cancel(t.Context(), e.leader, e.ev.ID, "", true); err != nil {
		t.Fatal(err)
	}
	if mails := e.events.StatusMails(e.ev.ID); len(mails) != 1 || mails[0].Kind != event.StatusMailCancelled {
		t.Fatalf("queued %+v", mails)
	}

	report := e.pass(t)
	if got, want := e.mail.to(mail.EventStatusCancelled), []string{"ada@example.com", "grace@example.com", "ikiz@example.com"}; !slices.Equal(got, want) {
		t.Fatalf("sent to %v, want %v (report %+v)", got, want, report)
	}
	if report.Mails != 1 || report.Sent != 3 || report.Skipped != 2 {
		t.Fatalf("report %+v", report)
	}
	for _, s := range e.mail.sent {
		if s.vars["EventName"] != "Gecekodu" || s.vars["EventStart"] != "14.03.2030 19:00" || s.vars["CancelReason"] != "Hava koşulları" || s.vars["FirstName"] == "" {
			t.Fatalf("vars %+v", s.vars)
		}
	}
	// Done with: another pass sends nothing.
	e.shift += time.Hour
	if report := e.pass(t); report.Sent != 0 || len(e.mail.sent) != 3 {
		t.Fatalf("second pass %+v", report)
	}
}

func TestCancellationWithoutNotifyQueuesNothing(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.member(t, "ada@example.com", "Ada")
	cancelled, err := e.svc.Cancel(t.Context(), e.leader, e.ev.ID, "", false)
	if err != nil || cancelled.CancelNotifiedAt != nil {
		t.Fatalf("%+v %v", cancelled.CancelNotifiedAt, err)
	}
	if mails := e.events.StatusMails(e.ev.ID); len(mails) != 0 {
		t.Fatalf("queued %+v", mails)
	}
	// Undoing with notify sends nothing: no cancellation mail went.
	if _, err := e.svc.Uncancel(t.Context(), e.leader, e.ev.ID, true); err != nil {
		t.Fatal(err)
	}
	if mails := e.events.StatusMails(e.ev.ID); len(mails) != 0 {
		t.Fatalf("queued after uncancel %+v", mails)
	}
}

// A failure a later try may pass keeps the mail for its backoff; whom it
// reached is not written to again. A refusal of one address does not hold
// the others back.
func TestFailedSendIsRetriedForTheOthersOnly(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.member(t, "ada@example.com", "Ada")
	e.guest(t, "grace@example.com", "Grace")
	e.guest(t, "bad@example.com", "Bad")
	e.mail.fail["grace@example.com"] = &mail.SendError{Status: 503}
	e.mail.fail["bad@example.com"] = &mail.SendError{Status: 422}
	if _, err := e.svc.Cancel(t.Context(), e.leader, e.ev.ID, "", true); err != nil {
		t.Fatal(err)
	}
	if report := e.pass(t); report.Retried != 1 || report.Mails != 0 {
		t.Fatalf("first pass %+v", report)
	}
	delete(e.mail.fail, "grace@example.com")
	if report := e.pass(t); report.Sent != 0 {
		t.Fatalf("before the backoff %+v", report)
	}
	e.shift += time.Hour
	report := e.pass(t)
	if report.Mails != 1 || report.Rejected != 1 {
		t.Fatalf("after the backoff %+v", report)
	}
	got := e.mail.to(mail.EventStatusCancelled)
	// Each once, whichever the first pass reached before its failure.
	if !slices.Equal(got, []string{"ada@example.com", "grace@example.com"}) {
		t.Fatalf("sent to %v", got)
	}
}

// Undone before the worker sent it, the cancellation mail is dropped.
func TestUndoneCancellationMailIsDropped(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.member(t, "ada@example.com", "Ada")
	if _, err := e.svc.Cancel(t.Context(), e.leader, e.ev.ID, "", true); err != nil {
		t.Fatal(err)
	}
	back, err := e.svc.Uncancel(t.Context(), e.leader, e.ev.ID, true)
	if err != nil || back.CancelNotifiedAt != nil {
		t.Fatalf("%+v %v", back.CancelNotifiedAt, err)
	}
	// The reinstated mail queued by the undo reaches nobody: the
	// cancellation mail reached nobody.
	report := e.pass(t)
	if len(e.mail.sent) != 0 || report.Dropped != 1 || report.Mails != 2 {
		t.Fatalf("report %+v, sent %+v", report, e.mail.sent)
	}
}

// The reinstated mail goes to whom the cancellation mail reached, and to
// nobody who came since or who did not get it.
func TestReinstatedMailGoesToWhomTheCancellationReached(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.member(t, "ada@example.com", "Ada")
	e.guest(t, "grace@example.com", "Grace")
	if _, err := e.svc.Cancel(t.Context(), e.leader, e.ev.ID, "", true); err != nil {
		t.Fatal(err)
	}
	e.pass(t)
	e.guest(t, "sonra@example.com", "Sonra")
	if _, err := e.svc.Uncancel(t.Context(), e.leader, e.ev.ID, true); err != nil {
		t.Fatal(err)
	}
	mails := e.events.StatusMails(e.ev.ID)
	if len(mails) != 2 || mails[1].Kind != event.StatusMailReinstated || mails[1].Follows == nil || *mails[1].Follows != mails[0].ID {
		t.Fatalf("queued %+v", mails)
	}
	e.pass(t)
	if got := e.mail.to(mail.EventStatusReinstated); !slices.Equal(got, []string{"ada@example.com", "grace@example.com"}) {
		t.Fatalf("reinstated to %v", got)
	}
	for _, s := range e.mail.sent {
		if _, has := s.vars["CancelReason"]; s.kind == mail.EventStatusReinstated && has {
			t.Fatalf("reinstated vars %+v", s.vars)
		}
	}

	// Cancelled and undone again without notify: nothing more.
	if _, err := e.svc.Cancel(t.Context(), e.leader, e.ev.ID, "", true); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Uncancel(t.Context(), e.leader, e.ev.ID, false); err != nil {
		t.Fatal(err)
	}
	if mails := e.events.StatusMails(e.ev.ID); len(mails) != 3 {
		t.Fatalf("queued %+v", mails)
	}
}

// A mail whose template key is empty is done with unsent.
func TestMailOffIsDoneWithUnsent(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.member(t, "ada@example.com", "Ada")
	e.mail.off[mail.EventStatusCancelled] = true
	if _, err := e.svc.Cancel(t.Context(), e.leader, e.ev.ID, "", true); err != nil {
		t.Fatal(err)
	}
	if report := e.pass(t); report.Off != 1 || len(e.mail.sent) != 0 {
		t.Fatalf("report %+v", report)
	}
	if report := e.pass(t); report.Mails != 0 {
		t.Fatalf("again %+v", report)
	}
}

func TestNewServiceNeedsItsParts(t *testing.T) {
	t.Parallel()
	if _, err := eventstatusmail.NewService(eventstatusmail.Config{}); err == nil {
		t.Fatal("an empty config was accepted")
	}
}
