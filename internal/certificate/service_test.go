package certificate_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/certificate"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

type recRender struct {
	html string
	pdf  []byte
}

func (r *recRender) PDF(_ context.Context, html string) ([]byte, error) {
	r.html = html
	if len(r.pdf) == 0 {
		return []byte("%PDF-1.4 artlab"), nil
	}
	return r.pdf, nil
}

type recMail struct {
	n     int
	email string
}

func (r *recMail) Certificate(_ context.Context, recipientEmail, _ string, _ map[string]string) {
	r.n++
	r.email = recipientEmail
}

func artlabSetup(t *testing.T, ratio float64) (event.Store, ticket.Store, user.Store, ticket.Service, certificate.Service, *recRender, *recMail, event.Event, ticket.Ticket, []event.Session, authz.Principal) {
	t.Helper()
	ctx := context.Background()
	events := event.NewMemoryStore()
	tickets := ticket.NewMemoryStore()
	users := user.NewMemoryStore()
	az := authz.NewAuthorizer(authz.DefaultPolicy())
	ticketSvc := ticket.NewService(tickets, events, az, users)
	render := &recRender{}
	mailer := &recMail{}
	certs := certificate.NewService(certificate.NewMemoryStore(), tickets, events, users, az, render, mailer, "https://api.example.test")

	r := ratio
	ev, err := events.Create(ctx, event.Event{
		Name: "ARTLAB 2026", Location: "YTÜ", OwnerTeam: "ARTLAB",
		AttendanceRule: certificate.RuleRatio, AttendanceRatio: &r,
	})
	if err != nil {
		t.Fatal(err)
	}
	day1, err := events.CreateDay(ctx, event.Day{EventID: ev.ID, Name: "Day 1"})
	if err != nil {
		t.Fatal(err)
	}
	day2, err := events.CreateDay(ctx, event.Day{EventID: ev.ID, Name: "Day 2"})
	if err != nil {
		t.Fatal(err)
	}
	sessions := make([]event.Session, 0, 8)
	for i := 0; i < 5; i++ {
		sess, err := events.CreateSession(ctx, event.Session{
			EventDayID: day1.ID, Title: "Talk", SpeakerName: "Ada", SessionType: "PRESENTATION", OrderIndex: i,
		})
		if err != nil {
			t.Fatal(err)
		}
		sessions = append(sessions, sess)
	}
	for i := 0; i < 3; i++ {
		sess, err := events.CreateSession(ctx, event.Session{
			EventDayID: day2.ID, Title: "Talk", SpeakerName: "Ada", SessionType: "PRESENTATION", OrderIndex: i,
		})
		if err != nil {
			t.Fatal(err)
		}
		sessions = append(sessions, sess)
	}

	ownerID := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	_, _, err = users.Upsert(ctx, user.User{ID: ownerID, Email: "ada@example.com", FirstName: "Ada", LastName: "Lovelace"})
	if err != nil {
		t.Fatal(err)
	}
	tk, err := ticketSvc.Apply(ctx, authz.Principal{ID: ownerID.String()}, ev.ID)
	if err != nil {
		t.Fatal(err)
	}
	leader := authz.Principal{ID: "lead", Groups: []string{"/UYELER/ARGE/ARTLAB/LIDERLER"}}
	return events, tickets, users, ticketSvc, certs, render, mailer, ev, tk, sessions, leader
}

func checkInN(t *testing.T, svc ticket.Service, leader authz.Principal, ticketID uuid.UUID, sessions []event.Session, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		if _, err := svc.CheckIn(context.Background(), leader, ticketID, sessions[i].ID); err != nil {
			t.Fatalf("check-in %d: %v", i, err)
		}
	}
}

func TestRecompute_ARTLABRatioSixIssuesFiveDoesNot(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	_, _, _, ticketSvc, certs, render, mailer, _, tk, sessions, leader := artlabSetup(t, 0.75)

	checkInN(t, ticketSvc, leader, tk.ID, sessions, 5)
	got, err := certs.RecomputeTicket(ctx, tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Fatalf("5 of 8 issued %+v", got)
	}
	if mailer.n != 0 {
		t.Fatalf("mail %d", mailer.n)
	}

	if _, err := ticketSvc.CheckIn(ctx, leader, tk.ID, sessions[5].ID); err != nil {
		t.Fatal(err)
	}
	issued, err := certs.RecomputeTicket(ctx, tk.ID)
	if err != nil || issued == nil {
		t.Fatalf("6 of 8: %v %+v", err, issued)
	}
	if issued.EventID != tk.EventID || issued.TicketID != tk.ID {
		t.Fatalf("issued %+v", issued)
	}
	if issued.RecipientEmail != "ada@example.com" || issued.RecipientName != "Ada Lovelace" {
		t.Fatalf("recipient %+v", issued)
	}
	if issued.Serial == "" || !strings.Contains(issued.VerifyURL, issued.Serial) {
		t.Fatalf("verify %s %s", issued.Serial, issued.VerifyURL)
	}
	if !strings.Contains(issued.VerifyURL, "https://api.example.test/v1/certificates/verify/") {
		t.Fatalf("verify url %s", issued.VerifyURL)
	}
	if issued.OwnerTeam != "ARTLAB" || issued.EventName != "ARTLAB 2026" {
		t.Fatalf("snapshot %+v", issued)
	}
	if !strings.Contains(render.html, "ARTLAB") || strings.Contains(strings.ToLower(render.html), "eventtype") {
		t.Fatalf("template html %s", render.html)
	}
	if mailer.n != 1 || mailer.email != "ada@example.com" {
		t.Fatalf("mail n=%d email=%s", mailer.n, mailer.email)
	}

	pdf, err := certs.PDF(ctx, issued.Serial)
	if err != nil {
		t.Fatal(err)
	}
	if string(pdf) != "%PDF-1.4 artlab" {
		t.Fatalf("pdf %q", pdf)
	}

	again, err := certs.RecomputeTicket(ctx, tk.ID)
	if err != nil || again != nil {
		t.Fatalf("second issue %v %+v", err, again)
	}
	if mailer.n != 1 {
		t.Fatalf("mail again %d", mailer.n)
	}
}

func TestRecompute_NoneWithSharedStores(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	events, tickets, users, ticketSvc, certs, _, mailer, ev, tk, sessions, leader := artlabSetup(t, 0.75)
	ev.AttendanceRule = certificate.RuleNone
	ev.AttendanceRatio = nil
	if _, err := events.Update(ctx, ev); err != nil {
		t.Fatal(err)
	}
	_ = tickets
	_ = users
	checkInN(t, ticketSvc, leader, tk.ID, sessions, 8)
	got, err := certs.RecomputeTicket(ctx, tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Fatalf("none issued %+v", got)
	}
	if mailer.n != 0 {
		t.Fatalf("mail %d", mailer.n)
	}
}

func TestRecompute_ZeroSessionsNoCertificate(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	events := event.NewMemoryStore()
	tickets := ticket.NewMemoryStore()
	users := user.NewMemoryStore()
	az := authz.NewAuthorizer(authz.DefaultPolicy())
	ticketSvc := ticket.NewService(tickets, events, az, users)
	certs := certificate.NewService(certificate.NewMemoryStore(), tickets, events, users, az, &recRender{}, &recMail{}, "")
	ev, err := events.Create(ctx, event.Event{
		Name: "Talk", Location: "YTÜ", OwnerTeam: "ARTLAB", AttendanceRule: certificate.RuleOnce,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := events.CreateDay(ctx, event.Day{EventID: ev.ID, Name: "Day 1"}); err != nil {
		t.Fatal(err)
	}
	ownerID := uuid.MustParse("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")
	if _, _, err := users.Upsert(ctx, user.User{ID: ownerID, Email: "b@example.com", FirstName: "B", LastName: "B"}); err != nil {
		t.Fatal(err)
	}
	tk, err := ticketSvc.Apply(ctx, authz.Principal{ID: ownerID.String()}, ev.ID)
	if err != nil {
		t.Fatal(err)
	}
	got, err := certs.RecomputeTicket(ctx, tk.ID)
	if err != nil || got != nil {
		t.Fatalf("zero sessions: %v %+v", err, got)
	}
}

func TestRecompute_OnceAfterOneOturum(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	events, tickets, users, ticketSvc, certs, _, _, ev, tk, sessions, leader := artlabSetup(t, 0.75)
	_ = tickets
	_ = users
	ev.AttendanceRule = certificate.RuleOnce
	ev.AttendanceRatio = nil
	if _, err := events.Update(ctx, ev); err != nil {
		t.Fatal(err)
	}
	got, err := certs.RecomputeTicket(ctx, tk.ID)
	if err != nil || got != nil {
		t.Fatalf("no check-in: %v %+v", err, got)
	}
	if _, err := ticketSvc.CheckIn(ctx, leader, tk.ID, sessions[0].ID); err != nil {
		t.Fatal(err)
	}
	issued, err := certs.RecomputeTicket(ctx, tk.ID)
	if err != nil || issued == nil {
		t.Fatalf("once: %v %+v", err, issued)
	}
}

func TestRecompute_CancelledAndDeletedExcluded(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	events, _, _, ticketSvc, certs, _, _, _, tk, sessions, leader := artlabSetup(t, 0.75)
	checkInN(t, ticketSvc, leader, tk.ID, sessions, 6)
	sessions[0].Cancelled = true
	if _, err := events.UpdateSession(ctx, sessions[0]); err != nil {
		t.Fatal(err)
	}
	got, err := certs.RecomputeTicket(ctx, tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Fatalf("cancelled talk still counted %+v", got)
	}

	sessions[0].Cancelled = false
	if _, err := events.UpdateSession(ctx, sessions[0]); err != nil {
		t.Fatal(err)
	}
	if err := events.DeleteSession(ctx, sessions[7].ID); err != nil {
		t.Fatal(err)
	}
	issued, err := certs.RecomputeTicket(ctx, tk.ID)
	if err != nil || issued == nil {
		t.Fatalf("deleted unused talk: %v %+v", err, issued)
	}
}

func TestVerifyAndRevoke(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	_, _, _, ticketSvc, certs, _, _, ev, tk, sessions, leader := artlabSetup(t, 0.75)
	checkInN(t, ticketSvc, leader, tk.ID, sessions, 6)
	issued, err := certs.RecomputeTicket(ctx, tk.ID)
	if err != nil || issued == nil {
		t.Fatal(err)
	}
	got, err := certs.Verify(ctx, issued.Serial)
	if err != nil {
		t.Fatal(err)
	}
	if got.Serial != issued.Serial || got.RevokedAt != nil {
		t.Fatalf("verify %+v", got)
	}
	member := authz.Principal{ID: "mem", Groups: []string{"/UYELER/ARGE/ARTLAB"}}
	if err := certs.Revoke(ctx, member, issued.Serial); !errors.Is(err, certificate.ErrForbidden) {
		t.Fatalf("member revoke %v", err)
	}
	if err := certs.Revoke(ctx, leader, issued.Serial); err != nil {
		t.Fatal(err)
	}
	revoked, err := certs.Verify(ctx, issued.Serial)
	if err != nil {
		t.Fatal(err)
	}
	if revoked.RevokedAt == nil {
		t.Fatal("expected revoked")
	}
	if _, err := certs.PDF(ctx, issued.Serial); !errors.Is(err, certificate.ErrNotFound) {
		t.Fatalf("revoked pdf %v", err)
	}
	_ = ev
}

func TestManualIssueAndAuthz(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	events, tickets, users, ticketSvc, certs, _, mailer, ev, tk, _, leader := artlabSetup(t, 0.75)
	member := authz.Principal{ID: "mem", Groups: []string{"/UYELER/ARGE/ARTLAB"}}
	if _, err := certs.Issue(ctx, member, ev.ID, tk.ID); !errors.Is(err, certificate.ErrForbidden) {
		t.Fatalf("member issue %v", err)
	}
	issued, err := certs.Issue(ctx, leader, ev.ID, tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if issued.TicketID != tk.ID {
		t.Fatalf("issued %+v", issued)
	}
	if mailer.n != 1 {
		t.Fatalf("mail %d", mailer.n)
	}
	if _, err := certs.Issue(ctx, leader, ev.ID, tk.ID); !errors.Is(err, certificate.ErrConflict) {
		t.Fatalf("dup %v", err)
	}

	empty, err := events.Create(ctx, event.Event{Name: "Seminar", Location: "YTÜ", AttendanceRule: certificate.RuleOnce})
	if err != nil {
		t.Fatal(err)
	}
	ownerID := uuid.MustParse("cccccccc-cccc-cccc-cccc-cccccccccccc")
	if _, _, err := users.Upsert(ctx, user.User{ID: ownerID, Email: "c@example.com", FirstName: "C", LastName: "C"}); err != nil {
		t.Fatal(err)
	}
	otherTk, err := ticketSvc.Apply(ctx, authz.Principal{ID: ownerID.String()}, empty.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := certs.Issue(ctx, leader, empty.ID, otherTk.ID); !errors.Is(err, certificate.ErrForbidden) {
		t.Fatalf("leader empty owner %v", err)
	}
	yk := authz.Principal{ID: "yk", Groups: []string{"/UYELER/YK"}}
	if _, err := certs.Issue(ctx, yk, empty.ID, otherTk.ID); err != nil {
		t.Fatalf("yk empty owner %v", err)
	}
	_ = tickets
}

func TestRecomputeEventIssuesEligible(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	_, _, _, ticketSvc, certs, _, mailer, ev, tk, sessions, leader := artlabSetup(t, 0.75)
	checkInN(t, ticketSvc, leader, tk.ID, sessions, 6)
	issued, err := certs.RecomputeEvent(ctx, leader, ev.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(issued) != 1 || issued[0].TicketID != tk.ID {
		t.Fatalf("issued %+v", issued)
	}
	if mailer.n != 1 {
		t.Fatalf("mail %d", mailer.n)
	}
	again, err := certs.RecomputeEvent(ctx, leader, ev.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 0 {
		t.Fatalf("again %+v", again)
	}
}

func TestMineAndGuestIssue(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	events, tickets, users, ticketSvc, certs, _, mailer, ev, _, sessions, leader := artlabSetup(t, 0.75)
	guest, err := ticketSvc.ApplyGuest(ctx, ev.ID, ticket.GuestInfo{
		FirstName: "Grace", LastName: "Hopper", Email: "grace@example.com", PhoneNumber: "555",
	})
	if err != nil {
		t.Fatal(err)
	}
	checkInN(t, ticketSvc, leader, guest.ID, sessions, 6)
	issued, err := certs.RecomputeTicket(ctx, guest.ID)
	if err != nil || issued == nil {
		t.Fatalf("guest %v %+v", err, issued)
	}
	if issued.RecipientEmail != "grace@example.com" || issued.RecipientName != "Grace Hopper" {
		t.Fatalf("guest recipient %+v", issued)
	}
	if mailer.email != "grace@example.com" {
		t.Fatalf("guest mail %s", mailer.email)
	}
	listed, err := certs.ListByEvent(ctx, leader, ev.ID)
	if err != nil || len(listed) != 1 {
		t.Fatalf("list %v %+v", err, listed)
	}
	_ = tickets
	_ = users
	_ = events
	ownerID := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	mine, err := certs.Mine(ctx, authz.Principal{ID: ownerID.String()})
	if err != nil {
		t.Fatal(err)
	}
	if len(mine) != 0 {
		t.Fatalf("guest cert is not the registered user's %+v", mine)
	}
}

func TestMailFailureStillIssues(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	events, tickets, users, ticketSvc, _, render, _, ev, tk, sessions, leader := artlabSetup(t, 0.75)
	_ = ev
	checkInN(t, ticketSvc, leader, tk.ID, sessions, 6)
	certs := certificate.NewService(certificate.NewMemoryStore(), tickets, events, users, authz.NewAuthorizer(authz.DefaultPolicy()), render, noopMail{}, "https://api.example.test")
	issued, err := certs.RecomputeTicket(ctx, tk.ID)
	if err != nil || issued == nil {
		t.Fatalf("issue despite mail: %v %+v", err, issued)
	}
}

type noopMail struct{}

func (noopMail) Certificate(context.Context, string, string, map[string]string) {}
