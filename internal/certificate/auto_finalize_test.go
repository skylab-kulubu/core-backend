package certificate_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/certificate"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// autoMail records the certificate mails and the publisher's mail.
type autoMail struct {
	recMail
	mu    sync.Mutex
	sent  []string
	count []string
}

func (m *autoMail) CertificateAutoFinalized(_ context.Context, recipientEmail, _ string, vars map[string]string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sent = append(m.sent, recipientEmail)
	m.count = append(m.count, vars["CertificateCount"])
}

// dueEvents is a plan that finds the Events it is given due.
type dueEvents struct{ ids []uuid.UUID }

func (d *dueEvents) AutoFinalizeDue(_ context.Context, _ time.Time, each func(uuid.UUID)) (bool, error) {
	for _, id := range d.ids {
		each(id)
	}
	return true, nil
}

// autoFixture is a memory certificate workspace with ARTLAB's template and
// one published Event per call of event, each with one Session.
type autoFixture struct {
	events  *event.MemoryStore
	tickets *ticket.MemoryStore
	users   *user.MemoryStore
	store   *certificate.MemoryStore
	mail    *autoMail
	plan    *dueEvents
	svc     certificate.Service
	leader  authz.Principal
	leadID  uuid.UUID
}

func newAutoFixture(t *testing.T) autoFixture {
	t.Helper()
	ctx := context.Background()
	f := autoFixture{
		events: event.NewMemoryStore(), tickets: ticket.NewMemoryStore(), users: user.NewMemoryStore(),
		store: certificate.NewMemoryStore(), mail: &autoMail{}, plan: &dueEvents{}, leadID: uuid.New(),
	}
	f.leader = authz.Principal{ID: f.leadID.String(), Groups: []string{"/UYELER/ARGE/ARTLAB/LIDERLER"}}
	if _, _, err := f.users.Upsert(ctx, user.User{ID: f.leadID, Email: "lider@example.com", FirstName: "Lider", LastName: "ARTLAB"}); err != nil {
		t.Fatal(err)
	}
	f.svc = certificate.NewServiceWithOptions(f.store, f.tickets, f.events, f.users, authz.NewAuthorizer(authz.DefaultPolicy()), &recRender{}, f.mail, certificate.Options{
		PublicAPIOrigin: "https://api.example.test", VerifyOrigin: "https://skyl.app/c", Templates: f.store, Jobs: f.store, AutoFinalize: f.plan,
	})
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

// event adds a published once Event of team, running from two days ago to
// two days on, published by the leader, with one counted Session ending at
// each of ends; it answers the first.
func (f autoFixture) event(t *testing.T, team string, auto bool, ends ...time.Time) (event.Event, event.Session) {
	t.Helper()
	ctx := context.Background()
	start, end := time.Now().Add(-48*time.Hour), time.Now().Add(48*time.Hour)
	ev, err := f.events.Create(ctx, event.Event{
		Name: "ARTLAB 2026", Location: "YTÜ", OwnerTeam: team, AttendanceRule: certificate.RuleOnce, AttendanceAutoFinalize: auto,
		Status: event.StatusPublished, StartDate: &start, EndDate: &end, PublishedBy: &f.leadID,
	})
	if err != nil {
		t.Fatal(err)
	}
	day, err := f.events.CreateDay(ctx, event.Day{EventID: ev.ID, Name: "Gün"})
	if err != nil {
		t.Fatal(err)
	}
	var first event.Session
	for i, at := range ends {
		sess, err := f.events.CreateSession(ctx, event.Session{EventDayID: day.ID, Title: "Oturum", SpeakerName: "Ada", SessionType: "PRESENTATION",
			OrderIndex: i, EndTime: &at, CertificateCounted: true})
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			first = sess
		}
	}
	return ev, first
}

// guest adds a Guest checked in on sess (none when sess is zero).
func (f autoFixture) guest(t *testing.T, ev event.Event, sess event.Session, first string) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	created, err := f.tickets.Create(ctx, ticket.Ticket{ID: uuid.New(), EventID: ev.ID, TicketType: ticket.Guest, GuestFirstName: first, GuestLastName: "Ak", GuestEmail: strings.ToLower(first) + "@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if sess.ID != uuid.Nil {
		if _, err := f.tickets.AddCheckIn(ctx, ticket.CheckIn{ID: uuid.New(), TicketID: created.ID, EventDayID: sess.EventDayID, SessionID: sess.ID}); err != nil {
			t.Fatal(err)
		}
	}
	return created.ID
}

// Manual finalization opens once the last live Session has ended, before
// the Event's end date (K7, S4): 409 while a live Session runs; a cancelled
// Session that would end later does not hold it.
func TestFinalizeOpensWhenTheLastLiveSessionEnds(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newAutoFixture(t)
	now := time.Now()
	over, _ := f.event(t, "ARTLAB", true, now.Add(-3*time.Hour), now.Add(-time.Minute))
	if _, err := f.svc.Finalize(ctx, f.leader, over.ID); err != nil {
		t.Fatalf("last Session over, end date to come: %v", err)
	}
	running, _ := f.event(t, "ARTLAB", true, now.Add(-3*time.Hour), now.Add(time.Hour))
	if _, err := f.svc.Finalize(ctx, f.leader, running.ID); !errors.Is(err, certificate.ErrConflict) {
		t.Fatalf("a Session still runs: %v", err)
	}
	cancelledLater, _ := f.event(t, "ARTLAB", true, now.Add(-2*time.Hour), now.Add(2*time.Hour))
	days, _ := f.events.ListDays(ctx, cancelledLater.ID)
	sessions, _ := f.events.ListSessions(ctx, days[0].ID)
	for _, sess := range sessions {
		if sess.EndTime.After(now) {
			sess.Cancelled = true
			if _, err := f.events.UpdateSession(ctx, sess); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := f.svc.Finalize(ctx, f.leader, cancelledLater.ID); err != nil {
		t.Fatalf("only a cancelled Session ends later: %v", err)
	}
	noSessions, _ := f.event(t, "ARTLAB", true)
	if _, err := f.svc.Finalize(ctx, f.leader, noSessions.ID); !errors.Is(err, certificate.ErrConflict) {
		t.Fatalf("no Session and the end date to come: %v", err)
	}
}

// A pass closes each due Event as the system (source auto, no one
// requested it) and tells the publisher how many certificates it queued. An
// Event whose template cannot be resolved is skipped with its error and
// tried again by the next pass; one whose switch went off meanwhile is
// passed over. The Summary shows when the attendance closes by itself, then
// who closed it.
func TestAutoFinalizeClosesDueEventsAndSkipsFailures(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newAutoFixture(t)
	now := time.Now()
	lastEnd := now.Add(-2 * time.Hour)
	due, sess := f.event(t, "ARTLAB", true, now.Add(-4*time.Hour), lastEnd)
	f.guest(t, due, sess, "Ayşe")
	f.guest(t, due, sess, "Burak")
	f.guest(t, due, event.Session{}, "Cem")
	broken, brokenSess := f.event(t, "GAMELAB", true, lastEnd)
	f.guest(t, broken, brokenSess, "Deniz")
	off, offSess := f.event(t, "ARTLAB", false, lastEnd)
	f.guest(t, off, offSess, "Ece")
	f.plan.ids = []uuid.UUID{broken.ID, due.ID, off.ID}

	summary, err := f.svc.Summary(ctx, f.leader, due.ID)
	if err != nil {
		t.Fatal(err)
	}
	if summary.AutoFinalizeAt == nil || !summary.AutoFinalizeAt.Equal(lastEnd.Add(certificate.AutoFinalizeDelay)) || summary.FinalizedSource != "" {
		t.Fatalf("before: autoFinalizeAt %v, want %v; source %q", summary.AutoFinalizeAt, lastEnd.Add(time.Hour), summary.FinalizedSource)
	}
	if summary, err := f.svc.Summary(ctx, f.leader, off.ID); err != nil || summary.AutoFinalizeAt != nil {
		t.Fatalf("switch off: %v %v", err, summary.AutoFinalizeAt)
	}

	result, err := f.svc.AutoFinalize(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Locked || result.Finalized != 1 || len(result.Errors) != 1 ||
		!strings.Contains(result.Errors[0].Error(), broken.ID.String()) || !strings.Contains(result.Errors[0].Error(), "dependency_not_found") {
		t.Fatalf("pass %+v", result)
	}
	batches, err := f.svc.ListBatches(ctx, f.leader, due.ID)
	if err != nil || len(batches) != 1 || batches[0].Reason != "finalization" || batches[0].RequestedBy != nil || batches[0].TotalCount != 2 {
		t.Fatalf("due batches %v %+v", err, batches)
	}
	summary, err = f.svc.Summary(ctx, f.leader, due.ID)
	if err != nil || summary.AttendanceFinalizedAt == nil || summary.FinalizedSource != certificate.FinalizedAuto || summary.AutoFinalizeAt != nil {
		t.Fatalf("after: %v %+v", err, summary)
	}
	if len(f.mail.sent) != 1 || f.mail.sent[0] != "lider@example.com" || f.mail.count[0] != "2" {
		t.Fatalf("publisher mails %v %v", f.mail.sent, f.mail.count)
	}
	for _, id := range []uuid.UUID{broken.ID, off.ID} {
		if batches, err := f.store.ListBatches(ctx, id); err != nil || len(batches) != 0 {
			t.Fatalf("%s batches %v %+v", id, err, batches)
		}
	}
	if metrics := f.svc.(interface{ Prometheus() string }).Prometheus(); !strings.Contains(metrics, "core_certificate_auto_finalized_total 1\n") ||
		!strings.Contains(metrics, "core_certificate_auto_finalize_errors_total 1\n") {
		t.Fatalf("metrics %s", metrics)
	}

	// GAMELAB gets a template; the next pass closes it, and the closed one
	// is passed over (its finalization conflicts).
	template, err := f.store.CreateTemplate(ctx, certificate.Template{ID: uuid.New(), Name: "GAMELAB", OwnerTeam: "GAMELAB", SourceKind: "figma", DraftLayout: versionedLayout("G")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.CreateVersion(ctx, certificate.TemplateVersion{ID: uuid.New(), TemplateID: template.ID, Layout: template.DraftLayout, Checksum: "g"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.SetBinding(ctx, certificate.Binding{ID: uuid.New(), Scope: certificate.ScopeOwnerTeam, ScopeKey: "GAMELAB", TemplateID: template.ID}); err != nil {
		t.Fatal(err)
	}
	result, err = f.svc.AutoFinalize(ctx, now.Add(time.Minute))
	if err != nil || result.Finalized != 1 || len(result.Errors) != 0 {
		t.Fatalf("second pass %v %+v", err, result)
	}
	if batches, err := f.svc.ListBatches(ctx, f.leader, due.ID); err != nil || len(batches) != 1 {
		t.Fatalf("the closed Event got another batch: %v %+v", err, batches)
	}
}

// A Session that ended less than AutoFinalizeDelay ago is not due yet, even
// when the plan names it (the service checks the time again).
func TestAutoFinalizeWaitsAnHourAfterTheLastSession(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newAutoFixture(t)
	now := time.Now()
	ev, sess := f.event(t, "ARTLAB", true, now.Add(-59*time.Minute))
	f.guest(t, ev, sess, "Ayşe")
	f.plan.ids = []uuid.UUID{ev.ID}
	if result, err := f.svc.AutoFinalize(ctx, now); err != nil || result.Finalized != 0 || len(result.Errors) != 0 {
		t.Fatalf("59 minutes: %v %+v", err, result)
	}
	if result, err := f.svc.AutoFinalize(ctx, now.Add(time.Minute)); err != nil || result.Finalized != 1 {
		t.Fatalf("an hour: %v %+v", err, result)
	}
}

// Rescan ("Uygunları yeniden tara") refuses an open attendance and who may
// not issue; once it is closed it queues only who is eligible now and has
// never had a certificate: not the issued, not the revoked, not one with a
// job waiting. With no one it opens no batch.
func TestRescanQueuesOnlyTheNewlyEligible(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newAutoFixture(t)
	now := time.Now()
	ev, sess := f.event(t, "ARTLAB", true, now.Add(-2*time.Hour))
	f.guest(t, ev, sess, "Ayşe")
	revoked := f.guest(t, ev, sess, "Burak")
	late := f.guest(t, ev, event.Session{}, "Cem")
	waiting := f.guest(t, ev, event.Session{}, "Deniz")

	if _, err := f.svc.Rescan(ctx, f.leader, ev.ID); !errors.Is(err, certificate.ErrConflict) {
		t.Fatalf("open attendance: %v", err)
	}
	if _, err := f.svc.Finalize(ctx, f.leader, ev.ID); err != nil {
		t.Fatal(err)
	}
	for {
		n, err := f.svc.ProcessNext(ctx, 10)
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			break
		}
	}
	certs, err := f.svc.ListByEvent(ctx, f.leader, ev.ID)
	if err != nil || len(certs) != 2 {
		t.Fatalf("issued %v %+v", err, certs)
	}
	for _, c := range certs {
		if c.TicketID == revoked {
			if err := f.svc.Revoke(ctx, f.leader, c.Serial); err != nil {
				t.Fatal(err)
			}
		}
	}
	// Cem and Deniz check in after the attendance closed; Deniz is queued
	// by hand and waits.
	for _, id := range []uuid.UUID{late, waiting} {
		if _, err := f.tickets.AddCheckIn(ctx, ticket.CheckIn{ID: uuid.New(), TicketID: id, EventDayID: sess.EventDayID, SessionID: sess.ID}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.svc.QueueManual(ctx, f.leader, ev.ID, waiting, ""); err != nil {
		t.Fatal(err)
	}

	member := authz.Principal{ID: uuid.NewString(), Groups: []string{"/UYELER/ARGE/ARTLAB"}}
	if _, err := f.svc.Rescan(ctx, member, ev.ID); !errors.Is(err, certificate.ErrForbidden) {
		t.Fatalf("member: %v", err)
	}
	result, err := f.svc.Rescan(ctx, f.leader, ev.ID)
	if err != nil {
		t.Fatal(err)
	}
	if result.Queued != 1 || result.Batch == nil || result.Batch.Reason != "rescan" || len(result.Batch.Jobs) != 1 || result.Batch.Jobs[0].TicketID != late {
		t.Fatalf("rescan %+v", result)
	}
	again, err := f.svc.Rescan(ctx, f.leader, ev.ID)
	if err != nil || again.Queued != 0 || again.Batch != nil {
		t.Fatalf("second rescan %v %+v", err, again)
	}
	batches, err := f.svc.ListBatches(ctx, f.leader, ev.ID)
	if err != nil || len(batches) != 3 {
		t.Fatalf("batches %v %d, want finalization, manual and one rescan", err, len(batches))
	}
}

// MaintainAutoFinalize runs a pass each interval and tells onError of a
// pass that failed and of each Event a pass could not close.
type passService struct {
	certificate.Service
	passes chan time.Time
}

func (s passService) AutoFinalize(_ context.Context, now time.Time) (certificate.AutoFinalizeResult, error) {
	select {
	case s.passes <- now:
	default:
	}
	return certificate.AutoFinalizeResult{Locked: true, Errors: []error{errors.New("event x: dependency_not_found")}}, errors.New("pass")
}

func TestMaintainAutoFinalizeReportsErrors(t *testing.T) {
	t.Parallel()
	svc := passService{passes: make(chan time.Time, 64)}
	ctx, cancel := context.WithCancel(context.Background())
	var mu sync.Mutex
	heard := []string{}
	done := certificate.MaintainAutoFinalize(ctx, svc, time.Millisecond, func(err error) {
		mu.Lock()
		defer mu.Unlock()
		heard = append(heard, err.Error())
	})
	<-svc.passes
	<-svc.passes
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("done did not close")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(heard) < 2 || heard[0] != "pass" || heard[1] != "event x: dependency_not_found" {
		t.Fatalf("heard %v", heard)
	}
}
