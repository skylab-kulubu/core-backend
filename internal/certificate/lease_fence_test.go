package certificate_test

import (
	"context"
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

// Two replicas share the queue. The first claims two reissue jobs and is still
// on the first when its lease runs out; the second claims both and finishes
// them. The first may then neither finish the job it has left nor put back the
// one it was on: each reissue sends one e-mail (ticket 25).
func TestStaleReplicaReissuesNothingAfterItsLeaseRanOut(t *testing.T) {
	ctx := context.Background()
	events := event.NewMemoryStore()
	tickets := ticket.NewMemoryStore()
	users := user.NewMemoryStore()
	store := certificate.NewMemoryStore()
	authorizer := authz.NewAuthorizer(authz.DefaultPolicy())
	ticketService := ticket.NewService(tickets, events, authorizer, users)
	options := certificate.Options{PublicAPIOrigin: "https://api.example.test", VerifyOrigin: "https://skyl.app/c", Templates: store, Jobs: store}
	staleMail, freshMail := &recMail{}, &recMail{}
	staleRender := &stallingRender{}
	stale := certificate.NewServiceWithOptions(store, tickets, events, users, authorizer, staleRender, staleMail, options)
	fresh := certificate.NewServiceWithOptions(store, tickets, events, users, authorizer, &recRender{}, freshMail, options)

	start, end := time.Now().Add(-time.Hour), time.Now().Add(24*time.Hour)
	ev, err := events.Create(ctx, event.Event{Name: "Kira Zirvesi", Location: "YTÜ", OwnerTeam: "ARTLAB", AttendanceRule: certificate.RuleOnce, StartDate: &start, EndDate: &end})
	if err != nil {
		t.Fatal(err)
	}
	day, _ := events.CreateDay(ctx, event.Day{EventID: ev.ID, Name: "Gün"})
	session, _ := events.CreateSession(ctx, event.Session{EventDayID: day.ID, Title: "Oturum", SpeakerName: "Ada", SessionType: "PRESENTATION"})
	ownerID := uuid.New()
	if _, _, err := users.Upsert(ctx, user.User{ID: ownerID, Email: "ada@example.com", FirstName: "Ada", LastName: "Lovelace"}); err != nil {
		t.Fatal(err)
	}
	item, err := ticketService.Apply(ctx, authz.Principal{ID: ownerID.String()}, ev.ID)
	if err != nil {
		t.Fatal(err)
	}
	leader := authz.Principal{ID: uuid.NewString(), Groups: []string{"/UYELER/ARGE/ARTLAB/LIDERLER"}}
	if _, err := ticketService.CheckIn(ctx, leader, item.ID, session.ID); err != nil {
		t.Fatal(err)
	}
	template, err := store.CreateTemplate(ctx, certificate.Template{ID: uuid.New(), Name: "ARTLAB", OwnerTeam: "ARTLAB", SourceKind: "figma", DraftLayout: versionedLayout("KIRA")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateVersion(ctx, certificate.TemplateVersion{ID: uuid.New(), TemplateID: template.ID, Layout: template.DraftLayout, Checksum: "kira"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetBinding(ctx, certificate.Binding{ID: uuid.New(), Scope: certificate.ScopeOwnerTeam, ScopeKey: "ARTLAB", TemplateID: template.ID}); err != nil {
		t.Fatal(err)
	}
	ended := time.Now().Add(-time.Minute)
	ev.EndDate = &ended
	if ev, err = events.Update(ctx, ev); err != nil {
		t.Fatal(err)
	}
	if _, err := fresh.Finalize(ctx, leader, ev.ID); err != nil {
		t.Fatal(err)
	}
	if processed, err := fresh.ProcessNext(ctx, 10); err != nil || processed != 1 {
		t.Fatalf("first issuance: processed=%d err=%v", processed, err)
	}
	issued, err := fresh.ListByEvent(ctx, leader, ev.ID)
	if err != nil || len(issued) != 1 {
		t.Fatalf("issued %v %+v", err, issued)
	}
	for range 2 {
		if _, err := fresh.Reissue(ctx, leader, issued[0].Serial); err != nil {
			t.Fatal(err)
		}
	}
	freshMail.n = 0

	staleRender.stall = func() {
		store.ExpireJobLeases()
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
	listed, err := fresh.ListByEvent(ctx, leader, ev.ID)
	if err != nil {
		t.Fatal(err)
	}
	active := 0
	for _, c := range listed {
		if c.RevokedAt == nil {
			active++
		}
	}
	if len(listed) != 3 || active != 1 {
		t.Fatalf("want the original and two reissues, one active; got %d certificates, %d active", len(listed), active)
	}
	if processed, err := fresh.ProcessNext(ctx, 10); err != nil || processed != 0 {
		t.Fatalf("a job came back to the queue: processed=%d err=%v", processed, err)
	}
}
