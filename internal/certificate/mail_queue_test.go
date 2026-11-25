package certificate_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/certificate"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/mail"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// queueMailer is a SkyMail that takes queued certificate mails: answer is
// what it says to the next send.
type queueMailer struct {
	mu     sync.Mutex
	answer error
	keys   []string
	to     []string
	direct int
	// during runs inside a send, once, outside the lock: a test's clock.
	during func()
}

func (m *queueMailer) Certificate(context.Context, string, string, map[string]string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.direct++
}

func (m *queueMailer) CertificateMail(_ context.Context, key, email, _ string, vars map[string]string) error {
	m.mu.Lock()
	during := m.during
	m.during = nil
	m.mu.Unlock()
	if during != nil {
		during()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.keys = append(m.keys, key)
	m.to = append(m.to, email+"|"+vars["Serial"])
	return m.answer
}

func (m *queueMailer) say(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.answer = err
}

func (m *queueMailer) sent() (keys, to []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.keys...), append([]string(nil), m.to...)
}

type mailFixture struct {
	store  *certificate.MemoryStore
	svc    certificate.Service
	mailer *queueMailer
	leader authz.Principal
	event  event.Event
	certs  []certificate.Certificate
}

// mailSetup issues two certificates for an ended Event through the queue.
func mailSetup(t *testing.T) mailFixture {
	t.Helper()
	return mailSetupWith(t, &queueMailer{}, &recRender{}, certificate.Options{})
}

// mailSetupWith is mailSetup with mailer, render and the options' MailPDF
// and Artifacts.
func mailSetupWith(t *testing.T, mailer certificate.Mailer, render *recRender, opts certificate.Options) mailFixture {
	t.Helper()
	ctx := context.Background()
	events := event.NewMemoryStore()
	tickets := ticket.NewMemoryStore()
	users := user.NewMemoryStore()
	store := certificate.NewMemoryStore()
	authorizer := authz.NewAuthorizer(authz.DefaultPolicy())
	ticketService := ticket.NewService(tickets, events, authorizer, users)
	svc := certificate.NewServiceWithOptions(store, tickets, events, users, authorizer, render, mailer, certificate.Options{
		PublicAPIOrigin: "https://api.example.test", VerifyOrigin: "https://skyl.app/c", Templates: store, Jobs: store,
		MailPDF: opts.MailPDF, Artifacts: opts.Artifacts,
	})
	start, end := time.Now().Add(-3*time.Hour), time.Now().Add(time.Hour)
	ev, err := events.Create(ctx, event.Event{Name: "ARTLAB", Location: "YTÜ", OwnerTeam: "ARTLAB", AttendanceRule: certificate.RuleOnce, StartDate: &start, EndDate: &end})
	if err != nil {
		t.Fatal(err)
	}
	day, _ := events.CreateDay(ctx, event.Day{EventID: ev.ID, Name: "Gün"})
	session, _ := events.CreateSession(ctx, event.Session{EventDayID: day.ID, Title: "Oturum", CertificateCounted: true, SpeakerName: "Ada", SessionType: "PRESENTATION"})
	leader := authz.Principal{ID: uuid.NewString(), Groups: []string{"/UYELER/ARGE/ARTLAB/LIDERLER"}}
	for i, email := range []string{"ada@example.com", "grace@example.com"} {
		id := uuid.New()
		if _, _, err := users.Upsert(ctx, user.User{ID: id, Email: email, FirstName: []string{"Ada", "Grace"}[i], LastName: "Test"}); err != nil {
			t.Fatal(err)
		}
		item, err := ticketService.Apply(ctx, authz.Principal{ID: id.String()}, ev.ID)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := ticketService.CheckIn(ctx, leader, item.ID, session.ID); err != nil {
			t.Fatal(err)
		}
	}
	// The Event ends: its attendance may be closed.
	ended := time.Now().Add(-time.Minute)
	ev.EndDate = &ended
	if ev, err = events.Update(ctx, ev); err != nil {
		t.Fatal(err)
	}
	template, err := store.CreateTemplate(ctx, certificate.Template{ID: uuid.New(), Name: "ARTLAB", OwnerTeam: "ARTLAB", SourceKind: "sky", DraftLayout: versionedLayout("ARTLAB")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateVersion(ctx, certificate.TemplateVersion{ID: uuid.New(), TemplateID: template.ID, Layout: template.DraftLayout, Checksum: "one"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetBinding(ctx, certificate.Binding{ID: uuid.New(), Scope: certificate.ScopeOwnerTeam, ScopeKey: "ARTLAB", TemplateID: template.ID}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Finalize(ctx, leader, ev.ID); err != nil {
		t.Fatal(err)
	}
	if processed, err := svc.ProcessNext(ctx, 10); err != nil || processed != 2 {
		t.Fatalf("issued %d %v", processed, err)
	}
	certs, err := svc.ListByEvent(ctx, leader, ev.ID)
	if err != nil || len(certs) != 2 {
		t.Fatalf("certificates %v %d", err, len(certs))
	}
	queue, _ := mailer.(*queueMailer)
	if withFiles, ok := mailer.(*fileMailer); ok {
		queue = &withFiles.queueMailer
	}
	return mailFixture{store: store, svc: svc, mailer: queue, leader: leader, event: ev, certs: certs}
}

func (f mailFixture) states(t *testing.T) map[string]certificate.MailState {
	t.Helper()
	certs, err := f.svc.ListByEvent(context.Background(), f.leader, f.event.ID)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]certificate.MailState{}
	for _, c := range certs {
		if c.Mail != nil {
			out[c.Serial] = *c.Mail
		}
	}
	return out
}

func (f mailFixture) process(t *testing.T) int {
	t.Helper()
	n, err := f.svc.ProcessMails(context.Background(), 20)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// Issuing queues the mail, it is not sent inline. While SkyMail is down the
// mail waits and is tried again under the same Idempotency-Key; once
// SkyMail is back it is sent.
func TestCertificateMailQueueRetriesUnderOneKey(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := mailSetup(t)
	if keys, _ := f.mailer.sent(); len(keys) != 0 || f.mailer.direct != 0 {
		t.Fatalf("issuing sent inline: %v direct=%d", keys, f.mailer.direct)
	}
	summary, err := f.svc.Summary(ctx, f.leader, f.event.ID)
	if err != nil || summary.MailQueuedCount != 2 || summary.MailFailedCount != 0 {
		t.Fatalf("summary %v %+v", err, summary)
	}

	f.mailer.say(&mail.SendError{Status: http.StatusServiceUnavailable})
	if n := f.process(t); n != 2 {
		t.Fatalf("claimed %d", n)
	}
	for serial, state := range f.states(t) {
		if state.Status != certificate.MailQueued || state.Attempts != 1 || state.ErrorCode != "skymail_503" {
			t.Fatalf("%s after SkyMail down: %+v", serial, state)
		}
	}
	if n := f.process(t); n != 0 {
		t.Fatalf("a mail was tried before its backoff passed: %d", n)
	}
	first, _ := f.mailer.sent()

	f.mailer.say(nil)
	f.store.MakeMailsDue()
	if n := f.process(t); n != 2 {
		t.Fatalf("claimed %d", n)
	}
	keys, to := f.mailer.sent()
	if len(keys) != 4 || keys[2] != first[0] && keys[2] != first[1] || keys[3] != first[0] && keys[3] != first[1] {
		t.Fatalf("keys %v", keys)
	}
	for _, key := range keys {
		if !strings.HasPrefix(key, certificate.MailIdempotencyKeyPrefix) {
			t.Fatalf("key %q", key)
		}
	}
	if !strings.HasPrefix(to[2], "ada@example.com|") && !strings.HasPrefix(to[2], "grace@example.com|") {
		t.Fatalf("to %v", to)
	}
	for serial, state := range f.states(t) {
		if state.Status != certificate.MailSent || state.SentAt == nil || state.ErrorCode != "" {
			t.Fatalf("%s after SkyMail back: %+v", serial, state)
		}
	}
	if n := f.process(t); n != 0 {
		t.Fatalf("a sent mail was claimed again: %d", n)
	}
	summary, _ = f.svc.Summary(ctx, f.leader, f.event.ID)
	if summary.MailQueuedCount != 0 || summary.MailFailedCount != 0 {
		t.Fatalf("summary %+v", summary)
	}
}

// A refused mail fails at once; one SkyMail never answers fails after
// MailRetryWindow. "Yeniden gönder" with scope failed queues those alone,
// under a new key, and pressing it twice queues once.
func TestCertificateMailResendFailed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := mailSetup(t)
	f.mailer.say(&mail.SendError{Status: http.StatusBadRequest})
	f.process(t)
	states := f.states(t)
	for serial, state := range states {
		if state.Status != certificate.MailFailed || state.ErrorCode != "skymail_400" || state.Attempts != 1 {
			t.Fatalf("%s refused: %+v", serial, state)
		}
	}
	// One of the two is sent by hand meanwhile: "all" would take it, "failed" not.
	f.mailer.say(nil)
	if got, err := f.svc.ResendMail(ctx, f.leader, f.certs[0].Serial); err != nil || got.Queued != 1 {
		t.Fatalf("resend one %v %+v", err, got)
	}
	if got, err := f.svc.ResendMail(ctx, f.leader, f.certs[0].Serial); err != nil || got.Queued != 0 || got.AlreadyQueued != 1 {
		t.Fatalf("resend one twice %v %+v", err, got)
	}
	f.process(t)
	if got := f.states(t)[f.certs[0].Serial]; got.Status != certificate.MailSent {
		t.Fatalf("resent %+v", got)
	}
	got, err := f.svc.ResendEventMails(ctx, f.leader, f.event.ID, certificate.ResendFailed)
	if err != nil || got.Queued != 1 || got.AlreadyQueued != 0 {
		t.Fatalf("resend failed %v %+v", err, got)
	}
	if again, err := f.svc.ResendEventMails(ctx, f.leader, f.event.ID, certificate.ResendFailed); err != nil || again.Queued != 0 {
		// The queued resend is now the latest mail: it is no longer failed.
		t.Fatalf("resend failed twice %v %+v", err, again)
	}
	keysBefore, _ := f.mailer.sent()
	f.process(t)
	keys, _ := f.mailer.sent()
	newKey := keys[len(keys)-1]
	for _, old := range keysBefore {
		if old == newKey {
			t.Fatalf("a resend reused the key %q", newKey)
		}
	}
	for serial, state := range f.states(t) {
		if state.Status != certificate.MailSent {
			t.Fatalf("%s %+v", serial, state)
		}
	}
	all, err := f.svc.ResendEventMails(ctx, f.leader, f.event.ID, certificate.ResendAll)
	if err != nil || all.Queued != 2 {
		t.Fatalf("resend all %v %+v", err, all)
	}
	// "Herkese" at most once per ResendAllCooldown for the Event.
	if _, err := f.svc.ResendEventMails(ctx, f.leader, f.event.ID, certificate.ResendAll); !errors.Is(err, certificate.ErrResendTooSoon) {
		t.Fatalf("resend all twice: %v", err)
	}

	// SkyMail never answers: tried again within the window, given up past it.
	f.mailer.say(&mail.SendError{})
	f.process(t)
	f.store.AgeMails(certificate.MailRetryWindow - 3*time.Hour)
	for range 2 {
		f.store.MakeMailsDue()
		f.process(t)
	}
	for serial, state := range f.states(t) {
		if state.Status != certificate.MailQueued || state.Attempts != 3 {
			t.Fatalf("%s within the window: %+v", serial, state)
		}
	}
	f.store.AgeMails(3 * time.Hour)
	f.store.MakeMailsDue()
	f.process(t)
	for serial, state := range f.states(t) {
		if state.Status != certificate.MailFailed || state.Attempts != 4 || state.ErrorCode != "skymail_unreachable" {
			t.Fatalf("%s given up: %+v", serial, state)
		}
	}
	f.store.MakeMailsDue()
	if n := f.process(t); n != 0 {
		t.Fatalf("a failed mail was claimed: %d", n)
	}
	summary, _ := f.svc.Summary(ctx, f.leader, f.event.ID)
	if summary.MailFailedCount != 2 {
		t.Fatalf("summary %+v", summary)
	}
	list, err := f.svc.Attendance(ctx, f.leader, f.event.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range list.Rows {
		if row.Certificate == nil || row.Certificate.Mail == nil || row.Certificate.Mail.Status != certificate.MailFailed {
			t.Fatalf("attendance row %+v", row)
		}
	}
}

// A certificate revoked before its mail goes out cancels the mail; one
// revoked cannot be resent. Only whoever may issue resends, and the scope
// is failed or all.
func TestCertificateMailRevokedAndRefused(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := mailSetup(t)
	if err := f.svc.Revoke(ctx, f.leader, f.certs[0].Serial); err != nil {
		t.Fatal(err)
	}
	f.process(t)
	_, to := f.mailer.sent()
	if len(to) != 1 || strings.HasSuffix(to[0], "|"+f.certs[0].Serial) {
		t.Fatalf("sent %v", to)
	}
	if _, err := f.svc.ResendMail(ctx, f.leader, f.certs[0].Serial); !errors.Is(err, certificate.ErrConflict) {
		t.Fatalf("resend revoked: %v", err)
	}
	stranger := authz.Principal{ID: uuid.NewString()}
	if _, err := f.svc.ResendMail(ctx, stranger, f.certs[1].Serial); !errors.Is(err, certificate.ErrForbidden) && !errors.Is(err, certificate.ErrNotFound) {
		t.Fatalf("stranger resend one: %v", err)
	}
	if _, err := f.svc.ResendEventMails(ctx, stranger, f.event.ID, certificate.ResendAll); !errors.Is(err, certificate.ErrForbidden) && !errors.Is(err, certificate.ErrNotFound) {
		t.Fatalf("stranger resend all: %v", err)
	}
	if _, err := f.svc.ResendEventMails(ctx, f.leader, f.event.ID, "everyone"); !errors.Is(err, certificate.ErrInvalid) {
		t.Fatalf("unknown scope: %v", err)
	}
	// The queue keeps one issued mail per certificate: a job that issues
	// again after a crash queues none.
	if written, err := f.store.EnqueueMail(ctx, certificate.Mail{ID: uuid.New(), CertificateID: f.certs[1].ID, EventID: f.event.ID, Reason: certificate.MailReasonIssued}); err != nil || written {
		t.Fatalf("second issued mail %v %v", written, err)
	}
}

// A Mailer that cannot take queued mails is sent to directly, as before.
func TestCertificateMailWithoutQueueSendsDirectly(t *testing.T) {
	t.Parallel()
	_, _, _, _, svc, _, mailer, ev, _, _, leader := artlabSetup(t, 0)
	if n, err := svc.ProcessMails(context.Background(), 10); err != nil || n != 0 {
		t.Fatalf("process %d %v", n, err)
	}
	if _, err := svc.ResendEventMails(context.Background(), leader, ev.ID, certificate.ResendAll); !errors.Is(err, certificate.ErrInvalid) {
		t.Fatalf("resend without a queue: %v", err)
	}
	_ = mailer
}

// A claim's lease covers its batch, and each send renews its own mail's
// first: a mail whose lease ran out while the ones before it were sent is
// not sent by that claim; the next claim sends it, under the same key.
func TestCertificateMailSkipsAMailWhoseLeaseRanOut(t *testing.T) {
	t.Parallel()
	f := mailSetup(t)
	f.mailer.during = f.store.ExpireMailLeases
	if n := f.process(t); n != 2 {
		t.Fatalf("claimed %d", n)
	}
	keys, _ := f.mailer.sent()
	if len(keys) != 1 {
		t.Fatalf("sent %v: the second mail's lease had run out", keys)
	}
	sent, queued := 0, 0
	for _, state := range f.states(t) {
		switch state.Status {
		case certificate.MailSent:
			sent++
		case certificate.MailQueued:
			queued++
		}
	}
	if sent != 1 || queued != 1 {
		t.Fatalf("sent %d queued %d", sent, queued)
	}
	if n := f.process(t); n != 1 {
		t.Fatalf("the skipped mail was not claimed again: %d", n)
	}
	if keys, _ := f.mailer.sent(); len(keys) != 2 || keys[0] == keys[1] {
		t.Fatalf("keys %v", keys)
	}
}

// A mail first tried longer ago than MailKeyLimit is not sent again:
// SkyMail may have forgotten its key.
func TestCertificateMailExpiresPastTheKeyLimit(t *testing.T) {
	t.Parallel()
	f := mailSetup(t)
	f.mailer.say(&mail.SendError{Status: http.StatusServiceUnavailable})
	f.process(t)
	f.mailer.say(nil)
	f.store.AgeMails(certificate.MailKeyLimit + time.Minute)
	f.store.MakeMailsDue()
	f.process(t)
	if keys, _ := f.mailer.sent(); len(keys) != 2 {
		t.Fatalf("an expired mail was sent: %v", keys)
	}
	for serial, state := range f.states(t) {
		if state.Status != certificate.MailFailed || state.ErrorCode != "expired" {
			t.Fatalf("%s %+v", serial, state)
		}
	}
}

// "Herkese yeniden gönder" opens again once the cooldown has passed.
func TestCertificateMailResendAllCooldown(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := mailSetup(t)
	if _, err := f.svc.ResendEventMails(ctx, f.leader, f.event.ID, certificate.ResendAll); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.ResendEventMails(ctx, f.leader, f.event.ID, certificate.ResendAll); !errors.Is(err, certificate.ErrResendTooSoon) {
		t.Fatalf("second: %v", err)
	}
	// Failed alone is not held back.
	if _, err := f.svc.ResendEventMails(ctx, f.leader, f.event.ID, certificate.ResendFailed); err != nil {
		t.Fatalf("failed: %v", err)
	}
	f.store.AgeResendAll(certificate.ResendAllCooldown)
	if got, err := f.svc.ResendEventMails(ctx, f.leader, f.event.ID, certificate.ResendAll); err != nil || got.AlreadyQueued != 2 {
		t.Fatalf("after the cooldown: %v %+v", err, got)
	}
}
