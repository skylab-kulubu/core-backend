package certificate_test

import (
	"bytes"
	"context"
	"errors"
	"log"
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

// stallingRender runs stall once, on the first PDF it is asked for: the
// replica holding the batch is still rendering while its lease runs out.
type stallingRender struct {
	recRender
	stall func()
}

func (r *stallingRender) PDF(ctx context.Context, html string) ([]byte, error) {
	if stall := r.stall; stall != nil {
		r.stall = nil
		stall()
	}
	return r.recRender.PDF(ctx, html)
}

// fenceFixture is one ended Event with one attendee whose Attendance counts,
// a bound template, and the shared certificate store two replicas use.
type fenceFixture struct {
	store   *certificate.MemoryStore
	options certificate.Options
	tickets ticket.Store
	events  event.Store
	users   user.Store
	authz   authz.Authorizer
	leader  authz.Principal
	ev      event.Event
}

func newFenceFixture(t *testing.T) fenceFixture {
	t.Helper()
	ctx := context.Background()
	f := fenceFixture{
		store:   certificate.NewMemoryStore(),
		tickets: ticket.NewMemoryStore(),
		events:  event.NewMemoryStore(),
		users:   user.NewMemoryStore(),
		authz:   authz.NewAuthorizer(authz.DefaultPolicy()),
		leader:  authz.Principal{ID: uuid.NewString(), Groups: []string{"/UYELER/ARGE/ARTLAB/LIDERLER"}},
	}
	f.options = certificate.Options{PublicAPIOrigin: "https://api.example.test", VerifyOrigin: "https://skyl.app/c", Templates: f.store, Jobs: f.store}
	ticketService := ticket.NewService(f.tickets, f.events, f.authz, f.users)
	start, end := time.Now().Add(-time.Hour), time.Now().Add(24*time.Hour)
	ev, err := f.events.Create(ctx, event.Event{Name: "Kira Zirvesi", Location: "YTÜ", OwnerTeam: "ARTLAB", AttendanceRule: certificate.RuleOnce, StartDate: &start, EndDate: &end})
	if err != nil {
		t.Fatal(err)
	}
	day, _ := f.events.CreateDay(ctx, event.Day{EventID: ev.ID, Name: "Gün"})
	session, _ := f.events.CreateSession(ctx, event.Session{EventDayID: day.ID, Title: "Oturum", SpeakerName: "Ada", SessionType: "PRESENTATION"})
	ownerID := uuid.New()
	if _, _, err := f.users.Upsert(ctx, user.User{ID: ownerID, Email: "ada@example.com", FirstName: "Ada", LastName: "Lovelace"}); err != nil {
		t.Fatal(err)
	}
	item, err := ticketService.Apply(ctx, authz.Principal{ID: ownerID.String()}, ev.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ticketService.CheckIn(ctx, f.leader, item.ID, session.ID); err != nil {
		t.Fatal(err)
	}
	template, err := f.store.CreateTemplate(ctx, certificate.Template{ID: uuid.New(), Name: "ARTLAB", OwnerTeam: "ARTLAB", SourceKind: "figma", DraftLayout: versionedLayout("KIRA")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.CreateVersion(ctx, certificate.TemplateVersion{ID: uuid.New(), TemplateID: template.ID, Layout: template.DraftLayout, Checksum: "kira"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.SetBinding(ctx, certificate.Binding{ID: uuid.New(), Scope: certificate.ScopeOwnerTeam, ScopeKey: "ARTLAB", TemplateID: template.ID}); err != nil {
		t.Fatal(err)
	}
	ended := time.Now().Add(-time.Minute)
	ev.EndDate = &ended
	if f.ev, err = f.events.Update(ctx, ev); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f fenceFixture) replica(render certificate.Renderer, mail *recMail, jobs certificate.JobStore) certificate.Service {
	options := f.options
	if jobs != nil {
		options.Jobs = jobs
	}
	return certificate.NewServiceWithOptions(f.store, f.tickets, f.events, f.users, f.authz, render, mail, options)
}

func (f fenceFixture) certificates(t *testing.T, s certificate.Service) (all, active int) {
	t.Helper()
	listed, err := s.ListByEvent(context.Background(), f.leader, f.ev.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range listed {
		if c.RevokedAt == nil {
			active++
		}
	}
	return len(listed), active
}

// Two replicas share the queue. The first claims two reissue jobs and is still
// on the first when its lease runs out; the second claims both and finishes
// them. The first may then neither finish the job it has left nor put back the
// one it was on: each reissue sends one e-mail (ticket 25).
func TestStaleReplicaReissuesNothingAfterItsLeaseRanOut(t *testing.T) {
	ctx := context.Background()
	f := newFenceFixture(t)
	staleMail, freshMail := &recMail{}, &recMail{}
	staleRender := &stallingRender{}
	stale := f.replica(staleRender, staleMail, nil)
	fresh := f.replica(&recRender{}, freshMail, nil)

	if _, err := fresh.Finalize(ctx, f.leader, f.ev.ID); err != nil {
		t.Fatal(err)
	}
	if processed, err := fresh.ProcessNext(ctx, 10); err != nil || processed != 1 {
		t.Fatalf("first issuance: processed=%d err=%v", processed, err)
	}
	issued, err := fresh.ListByEvent(ctx, f.leader, f.ev.ID)
	if err != nil || len(issued) != 1 {
		t.Fatalf("issued %v %+v", err, issued)
	}
	for range 2 {
		if _, err := fresh.Reissue(ctx, f.leader, issued[0].Serial); err != nil {
			t.Fatal(err)
		}
	}
	freshMail.n = 0

	staleRender.stall = func() {
		f.store.ExpireJobLeases()
		if processed, err := fresh.ProcessNext(ctx, 10); err != nil || processed != 2 {
			t.Errorf("fresh replica: processed=%d err=%v", processed, err)
		}
	}
	if processed, err := stale.ProcessNext(ctx, 10); err != nil || processed != 2 {
		t.Fatalf("stale replica: processed=%d err=%v", processed, err)
	}

	if staleMail.n != 0 || freshMail.n != 2 {
		t.Fatalf("e-mails: stale replica %d, fresh replica %d; want 0 and 2", staleMail.n, freshMail.n)
	}
	if all, active := f.certificates(t, fresh); all != 3 || active != 1 {
		t.Fatalf("want the original and two reissues, one active; got %d certificates, %d active", all, active)
	}
	if processed, err := fresh.ProcessNext(ctx, 10); err != nil || processed != 0 {
		t.Fatalf("a job came back to the queue: processed=%d err=%v", processed, err)
	}
}

// lateCompletion runs late once, just before the first completion: the replica
// wrote the certificate, and its lease ran out before it could finish the job.
type lateCompletion struct {
	*certificate.MemoryStore
	late func()
}

func (l *lateCompletion) CompleteJob(ctx context.Context, jobID, leaseToken, certificateID uuid.UUID) error {
	if late := l.late; late != nil {
		l.late = nil
		late()
	}
	return l.MemoryStore.CompleteJob(ctx, jobID, leaseToken, certificateID)
}

// The stale replica wrote the certificate but lost the job before it could
// complete it. The replica that claims the job next finds the certificate this
// job wrote: it completes the job and sends the one e-mail, and issues nothing
// again, for a first issue and for a reissue alike (ticket 25, review B2).
func TestJobCompletedByTheNextClaimWhenItsCertificateWasWrittenLate(t *testing.T) {
	for _, reissue := range []bool{false, true} {
		t.Run(map[bool]string{false: "issue", true: "reissue"}[reissue], func(t *testing.T) {
			ctx := context.Background()
			f := newFenceFixture(t)
			staleMail, freshMail := &recMail{}, &recMail{}
			late := &lateCompletion{MemoryStore: f.store}
			stale := f.replica(&recRender{}, staleMail, late)
			fresh := f.replica(&recRender{}, freshMail, nil)

			if _, err := fresh.Finalize(ctx, f.leader, f.ev.ID); err != nil {
				t.Fatal(err)
			}
			wantAll := 1
			if reissue {
				if _, err := fresh.ProcessNext(ctx, 10); err != nil {
					t.Fatal(err)
				}
				issued, err := fresh.ListByEvent(ctx, f.leader, f.ev.ID)
				if err != nil || len(issued) != 1 {
					t.Fatalf("issued %v %+v", err, issued)
				}
				if _, err := fresh.Reissue(ctx, f.leader, issued[0].Serial); err != nil {
					t.Fatal(err)
				}
				freshMail.n = 0
				wantAll = 2
			}

			late.late = func() {
				f.store.ExpireJobLeases()
				if processed, err := fresh.ProcessNext(ctx, 10); err != nil || processed != 1 {
					t.Errorf("fresh replica: processed=%d err=%v", processed, err)
				}
			}
			if _, err := stale.ProcessNext(ctx, 10); err != nil {
				t.Fatal(err)
			}

			if staleMail.n != 0 || freshMail.n != 1 {
				t.Fatalf("e-mails: stale replica %d, fresh replica %d; want 0 and 1", staleMail.n, freshMail.n)
			}
			if all, active := f.certificates(t, fresh); all != wantAll || active != 1 {
				t.Fatalf("got %d certificates, %d active; want %d and 1", all, active, wantAll)
			}
			if processed, err := fresh.ProcessNext(ctx, 10); err != nil || processed != 0 {
				t.Fatalf("a job came back to the queue: processed=%d err=%v", processed, err)
			}
		})
	}
}

// brokenDatabase answers the queue's writes with the text pgx gives when the
// pool cannot open a connection.
type brokenDatabase struct {
	*certificate.MemoryStore
	renew, batch, fail bool
}

var errNoConnection = errors.New("failed to connect to `user=core database=core`: 10.0.0.7:5432 (db): dial error")

func (b *brokenDatabase) RenewJobLease(ctx context.Context, jobID, leaseToken uuid.UUID, lease time.Duration) error {
	if b.renew {
		return errNoConnection
	}
	return b.MemoryStore.RenewJobLease(ctx, jobID, leaseToken, lease)
}

func (b *brokenDatabase) GetBatch(ctx context.Context, id uuid.UUID) (certificate.Batch, error) {
	if b.batch {
		return certificate.Batch{}, errNoConnection
	}
	return b.MemoryStore.GetBatch(ctx, id)
}

func (b *brokenDatabase) FailJob(ctx context.Context, jobID, leaseToken uuid.UUID, errorCode string, retryAt time.Time, terminal bool) error {
	if b.fail {
		return errNoConnection
	}
	return b.MemoryStore.FailJob(ctx, jobID, leaseToken, errorCode, retryAt, terminal)
}

// A lease that cannot be renewed and a job that cannot be put back are logged
// with the job and the error's class: the pool's connection error names the
// user, the database and the address (review T2).
func TestQueueLogsNoConnectionDetails(t *testing.T) {
	for _, broken := range []brokenDatabase{{renew: true}, {batch: true, fail: true}} {
		ctx := context.Background()
		f := newFenceFixture(t)
		broken.MemoryStore = f.store
		replica := f.replica(&recRender{}, &recMail{}, &broken)
		if _, err := replica.Finalize(ctx, f.leader, f.ev.ID); err != nil {
			t.Fatal(err)
		}
		var logs bytes.Buffer
		previous := log.Writer()
		log.SetOutput(&logs)
		_, err := replica.ProcessNext(ctx, 10)
		log.SetOutput(previous)
		if err != nil {
			t.Fatal(err)
		}
		line := logs.String()
		if strings.Count(line, "ERROR certificate job") != 1 {
			t.Fatalf("log %q, want one line", line)
		}
		for _, secret := range []string{"user=", "database=", "10.0.0.7", "dial error"} {
			if strings.Contains(line, secret) {
				t.Fatalf("log %q names %q", line, secret)
			}
		}
	}
}
