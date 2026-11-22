package ticket_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
	"github.com/skylab-kulubu/core-backend/internal/testpostgres"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
	"github.com/skylab-kulubu/core-backend/internal/ticketqr"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

type resendEnv struct {
	pool   *pgxpool.Pool
	store  *ticket.PostgresStore
	events *event.PostgresStore
	users  *user.PostgresStore
	signer *ticketqr.Signer
	svc    ticket.Service
	ev     event.Event
	sess   event.Session
	staff  authz.Principal
	guest  ticket.Ticket
}

func newResendEnv(t *testing.T) *resendEnv {
	t.Helper()
	pool := testpostgres.Start(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	e := &resendEnv{pool: pool, store: ticket.NewPostgresStore(pool), events: event.NewPostgresStore(pool), users: user.NewPostgresStore(pool),
		signer: ticketqr.NewSigner(ticketqr.DeriveKey(key), ticketqr.Config{})}
	e.store.NotifyCreated(true)
	e.svc = ticket.NewService(e.store, e.events, authz.NewAuthorizer(authz.DefaultPolicy()), e.users, e.signer)
	staffID := uuid.New()
	if _, _, err := user.NewService(e.users).Ensure(ctx, staffID, user.Profile{Email: "staff@example.com", FirstName: "Kapı", LastName: "Görevlisi"}); err != nil {
		t.Fatal(err)
	}
	e.staff = authz.Principal{ID: staffID.String(), Groups: []string{"/UYELER/ARGE/WEBLAB"}}
	end := time.Now().Add(72 * time.Hour)
	e.ev, err = e.events.Create(ctx, event.Event{Name: "Hack", Location: "YTÜ", OwnerTeam: "WEBLAB", EndDate: &end, DoorStaffIDs: []uuid.UUID{staffID}})
	if err != nil {
		t.Fatal(err)
	}
	day := seedDay(t, e.events, e.ev.ID, "Gün 1")
	e.sess = seedSession(t, e.events, day.ID, "Açılış")
	applied, err := e.svc.ApplyGuest(ctx, weblabLead, e.ev.ID, ticket.GuestInfo{FirstName: "Ada", LastName: "Lovelace", Email: "ada@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	e.guest = applied.Ticket
	// The first mail went out.
	if _, err := pool.Exec(ctx, `DELETE FROM ticket_notifications`); err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *resendEnv) queued(t *testing.T, id uuid.UUID) (int, bool) {
	t.Helper()
	var resend int
	err := e.pool.QueryRow(context.Background(), `SELECT resend FROM ticket_notifications WHERE ticket_id = $1`, id).Scan(&resend)
	if err != nil {
		return 0, false
	}
	return resend, true
}

func (e *resendEnv) resends(t *testing.T, id uuid.UUID) int {
	t.Helper()
	var n int
	if err := e.pool.QueryRow(context.Background(), `SELECT count(*) FROM ticket_resends WHERE ticket_id = $1`, id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// Who may send a Ticket again or renew its link: who may give out the
// Event's Tickets (its team's Leaders, Privileged with ticket:manage). Door staff, DK and
// the holder may not.
func TestResendIsForWhoGivesOutTickets(t *testing.T) {
	e := newResendEnv(t)
	ctx := context.Background()
	holderID := uuid.New()
	if _, _, err := user.NewService(e.users).Ensure(ctx, holderID, user.Profile{Email: "grace@example.com", FirstName: "Grace", LastName: "Hopper"}); err != nil {
		t.Fatal(err)
	}
	holder := authz.Principal{ID: holderID.String()}
	member, err := e.svc.Apply(ctx, holder, e.ev.ID)
	if err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		p    authz.Principal
		want error
	}{
		"Leader": {weblabLead, nil},
		// Privileged: ticket:manage in the role modes, the group in the
		// groups mode (the default).
		"YK (ticket:manage)": {yk, nil},
		"door staff":         {e.staff, ticket.ErrForbidden},
		"DK":                 {authz.Principal{ID: "dk", Groups: []string{"/UYELER/DK"}}, ticket.ErrForbidden},
		"the holder":         {holder, ticket.ErrForbidden},
		"another team":       {otherMember, ticket.ErrForbidden},
	} {
		_, err := e.svc.ResendTicket(ctx, tc.p, member.ID, "")
		if !errors.Is(err, tc.want) {
			t.Errorf("resend by %s: %v, want %v", name, err, tc.want)
		}
		_, _, err = e.svc.RenewTicketLink(ctx, tc.p, member.ID, false, "")
		if !errors.Is(err, tc.want) {
			t.Errorf("renew by %s: %v, want %v", name, err, tc.want)
		}
	}
}

// A resend queues the Ticket's mail under the next resend number; the
// same Idempotency-Key writes nothing again; three an hour.
func TestResendQueuesUnderTheNextNumber(t *testing.T) {
	e := newResendEnv(t)
	ctx := context.Background()
	first, err := e.svc.ResendTicket(ctx, weblabLead, e.guest.ID, "istek-1")
	if err != nil || first.N != 1 || first.Replayed || !first.Queued {
		t.Fatalf("first %+v %v", first, err)
	}
	if n, ok := e.queued(t, e.guest.ID); !ok || n != 1 {
		t.Fatalf("queued %d %v", n, ok)
	}
	again, err := e.svc.ResendTicket(ctx, weblabLead, e.guest.ID, "istek-1")
	if err != nil || again.N != 1 || !again.Replayed {
		t.Fatalf("again %+v %v", again, err)
	}
	if e.resends(t, e.guest.ID) != 1 {
		t.Fatal("a repeated request wrote a second row")
	}
	// A second resend while the first still waits takes the row over.
	second, err := e.svc.ResendTicket(ctx, weblabLead, e.guest.ID, "")
	if err != nil || second.N != 2 {
		t.Fatalf("second %+v %v", second, err)
	}
	if n, _ := e.queued(t, e.guest.ID); n != 2 {
		t.Fatalf("queued resend %d", n)
	}
	if _, err := e.svc.ResendTicket(ctx, weblabLead, e.guest.ID, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.ResendTicket(ctx, weblabLead, e.guest.ID, ""); !errors.Is(err, ticket.ErrResendLimited) {
		t.Fatalf("fourth in an hour: %v", err)
	}
	// An hour on, again.
	if _, err := e.pool.Exec(ctx, `UPDATE ticket_resends SET created_at = created_at - interval '61 minutes'`); err != nil {
		t.Fatal(err)
	}
	if got, err := e.svc.ResendTicket(ctx, weblabLead, e.guest.ID, ""); err != nil || got.N != 4 {
		t.Fatalf("an hour on: %+v %v", got, err)
	}
	if _, err := ticket.RequestHash(string(make([]byte, 129))); !errors.Is(err, ticket.ErrInvalid) {
		t.Fatal("a 129-character Idempotency-Key")
	}
}

// Only a published Event that is not over, with the Notifications on, sends
// a Ticket again; an erased holder's Ticket is sent to nobody.
func TestResendRefusals(t *testing.T) {
	e := newResendEnv(t)
	ctx := context.Background()

	e.store.NotifyCreated(false)
	if _, err := e.svc.ResendTicket(ctx, weblabLead, e.guest.ID, ""); !errors.Is(err, ticket.ErrNotificationOff) {
		t.Fatalf("off: %v", err)
	}
	e.store.NotifyCreated(true)

	past := time.Now().Add(-48 * time.Hour)
	ended, err := e.events.Create(ctx, event.Event{Name: "Bitti", Location: "YTÜ", OwnerTeam: "WEBLAB", StartDate: &past, EndDate: &past})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now().Add(48 * time.Hour)
	draft, err := e.events.Create(ctx, event.Event{Name: "Taslak", Location: "YTÜ", OwnerTeam: "WEBLAB", StartDate: &start, Status: event.StatusDraft})
	if err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		ev   event.Event
		want error
	}{
		"ended": {ended, ticket.ErrEventEnded},
		"draft": {draft, ticket.ErrEventNotPublished},
	} {
		applied, err := e.svc.ApplyGuest(ctx, weblabLead, tc.ev.ID, ticket.GuestInfo{FirstName: "Ada", LastName: "L", Email: uuid.NewString() + "@example.com"})
		if err != nil {
			// An ended Event takes no Guest apply from the panel either way.
			created, cerr := e.store.Create(ctx, ticket.Ticket{EventID: tc.ev.ID, TicketType: ticket.Guest, GuestFirstName: "Ada", GuestEmail: uuid.NewString() + "@example.com"})
			if cerr != nil {
				t.Fatalf("%s: %v / %v", name, err, cerr)
			}
			applied.Ticket = created
		}
		if _, err := e.svc.ResendTicket(ctx, weblabLead, applied.Ticket.ID, ""); !errors.Is(err, tc.want) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := event.NewService(e.events, authz.NewAuthorizer(authz.DefaultPolicy())).Cancel(ctx, weblabLead, e.ev.ID, "", false); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.ResendTicket(ctx, weblabLead, e.guest.ID, ""); !errors.Is(err, ticket.ErrEventCancelled) {
		t.Fatalf("cancelled: %v", err)
	}
	if _, err := e.pool.Exec(ctx, `UPDATE tickets SET guest_email = '', access_key = NULL WHERE id = $1`, e.guest.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.svc.RenewTicketLink(ctx, weblabLead, e.guest.ID, false, ""); !errors.Is(err, ticket.ErrHolderErased) {
		t.Fatalf("erased: %v", err)
	}
}

// Renewing the link: a new key and QR version, the same code; the old page
// is not found and the old Ticket QR is refused (410); the mail goes again
// with the new link, unless notify is false.
func TestRenewTicketLink(t *testing.T) {
	e := newResendEnv(t)
	ctx := context.Background()
	oldQR, err := e.svc.TicketQR(e.guest, e.ev)
	if err != nil {
		t.Fatal(err)
	}
	renewed, sent, err := e.svc.RenewTicketLink(ctx, weblabLead, e.guest.ID, true, "yenile-1")
	if err != nil || !sent.Queued || sent.N != 1 {
		t.Fatalf("renew %+v %+v %v", renewed, sent, err)
	}
	if renewed.AccessKey == e.guest.AccessKey || renewed.QRVersion != 2 || renewed.Code != e.guest.Code {
		t.Fatalf("renewed %q v%d %q", renewed.AccessKey, renewed.QRVersion, renewed.Code)
	}
	if _, err := e.store.GetByAccessKey(ctx, e.guest.AccessKey); !errors.Is(err, ticket.ErrNotFound) {
		t.Fatalf("old key: %v", err)
	}
	if got, err := e.store.GetByAccessKey(ctx, renewed.AccessKey); err != nil || got.ID != e.guest.ID {
		t.Fatalf("new key: %v", err)
	}
	if n, ok := e.queued(t, e.guest.ID); !ok || n != 1 {
		t.Fatal("the mail with the new link is not queued")
	}
	if _, err := e.svc.CheckInTicketQR(ctx, e.staff, e.sess.ID, oldQR); !errors.Is(err, ticket.ErrTicketQRSuperseded) {
		t.Fatalf("old QR: %v", err)
	}
	newQR, err := e.svc.TicketQR(renewed, e.ev)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.CheckInTicketQR(ctx, e.staff, e.sess.ID, newQR); err != nil {
		t.Fatalf("new QR: %v", err)
	}
	// A repeated request renews nothing again.
	again, sentAgain, err := e.svc.RenewTicketLink(ctx, weblabLead, e.guest.ID, true, "yenile-1")
	if err != nil || !sentAgain.Replayed || again.AccessKey != renewed.AccessKey || again.QRVersion != 2 {
		t.Fatalf("again %+v %+v %v", again, sentAgain, err)
	}

	// notify=false: no mail, the Notifications may be off; with them off
	// and notify, nothing changes.
	e.store.NotifyCreated(false)
	if _, _, err := e.svc.RenewTicketLink(ctx, weblabLead, e.guest.ID, true, ""); !errors.Is(err, ticket.ErrNotificationOff) {
		t.Fatalf("notify while off: %v", err)
	}
	if got, _ := e.store.Get(ctx, e.guest.ID); got.QRVersion != 2 {
		t.Fatalf("a refused renewal renewed: v%d", got.QRVersion)
	}
	if _, err := e.pool.Exec(ctx, `DELETE FROM ticket_notifications`); err != nil {
		t.Fatal(err)
	}
	quiet, sentQuiet, err := e.svc.RenewTicketLink(ctx, weblabLead, e.guest.ID, false, "")
	if err != nil || sentQuiet.Queued || quiet.QRVersion != 3 {
		t.Fatalf("quiet %+v %+v %v", quiet, sentQuiet, err)
	}
	if _, ok := e.queued(t, e.guest.ID); ok {
		t.Fatal("notify=false queued a mail")
	}
}
