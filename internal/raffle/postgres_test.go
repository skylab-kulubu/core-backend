package raffle_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/certificate"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
	"github.com/skylab-kulubu/core-backend/internal/raffle"
	"github.com/skylab-kulubu/core-backend/internal/teamaudit"
	"github.com/skylab-kulubu/core-backend/internal/testpostgres"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// The Postgres store over a real schema: the candidates are the active
// Tickets with their live Sessions and entry days, the attendance is the
// certificate store's, a draw and a forfeit keep and answer the record,
// two draws at once never share a winner, and the Team audit record takes
// the raffle rows on an event editor's basis.
func TestPostgresRaffle(t *testing.T) {
	pool := testpostgres.Start(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	ev, err := event.NewPostgresStore(pool).Create(ctx, event.Event{Name: "ARTLAB", Location: "YTÜ", OwnerTeam: "YK", AttendanceRule: certificate.RuleCount})
	if err != nil {
		t.Fatal(err)
	}
	dayID := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO event_days (id, event_id, name, start_date) VALUES ($1, $2, 'Gün', '2026-11-27 08:00+00')`, dayID, ev.ID); err != nil {
		t.Fatal(err)
	}
	sessions := make([]uuid.UUID, 5)
	for i := range sessions {
		sessions[i] = uuid.New()
		cancelled := i == 4 // a cancelled Session is not live
		if _, err := pool.Exec(ctx, `INSERT INTO sessions (id, event_day_id, title, session_type, order_index, certificate_counted, cancelled)
			VALUES ($1, $2, 'Oturum', 'TALK', $3, true, $4)`, sessions[i], dayID, i, cancelled); err != nil {
			t.Fatal(err)
		}
	}
	users := user.NewPostgresStore(pool)
	ticketOf := map[string]uuid.UUID{}
	person := func(name string, guest bool, status string, checkins int, entered bool) {
		t.Helper()
		id := uuid.New()
		ticketOf[name] = id
		if guest {
			if _, err := pool.Exec(ctx, `INSERT INTO tickets (id, event_id, ticket_type, guest_first_name, guest_last_name, guest_email) VALUES ($1, $2, 'GUEST', $3, 'Misafir', $4)`,
				id, ev.ID, name, name+"@Example.com"); err != nil {
				t.Fatal(err)
			}
		} else {
			owner := uuid.New()
			if _, _, err := users.Upsert(ctx, user.User{ID: owner, Email: name + "@example.com", FirstName: name, LastName: "Üye"}); err != nil {
				t.Fatal(err)
			}
			if _, err := pool.Exec(ctx, `INSERT INTO tickets (id, event_id, ticket_type, owner_id) VALUES ($1, $2, 'REGISTERED', $3)`, id, ev.ID, owner); err != nil {
				t.Fatal(err)
			}
		}
		for i := range checkins {
			if _, err := pool.Exec(ctx, `INSERT INTO ticket_checkins (id, ticket_id, event_day_id, session_id) VALUES ($1, $2, $3, $4)`, uuid.New(), id, dayID, sessions[i]); err != nil {
				t.Fatal(err)
			}
		}
		if entered {
			if _, err := pool.Exec(ctx, `INSERT INTO venue_entries (id, ticket_id, event_id, entry_date, source) VALUES ($1, $2, $3, '2026-11-27', 'scan')`, uuid.New(), id, ev.ID); err != nil {
				t.Fatal(err)
			}
		}
		if status != "active" {
			if _, err := pool.Exec(ctx, `UPDATE tickets SET status = $2, cancelled_at = CASE WHEN $2 = 'cancelled' THEN now() END,
				cancelled_by = CASE WHEN $2 = 'cancelled' THEN 'operator' END, waitlisted_at = CASE WHEN $2 = 'waitlisted' THEN now() END WHERE id = $1`, id, status); err != nil {
				t.Fatal(err)
			}
		}
	}
	person("ada", false, "active", 5, true) // 4 live check-ins (the fifth Session is cancelled)
	person("alan", false, "active", 3, true)
	person("grace", false, "active", 1, false)
	person("edsger", true, "active", 4, true)
	person("barbara", true, "active", 0, false)
	person("iptal", false, "cancelled", 5, true)
	person("bekleyen", true, "waitlisted", 5, true)

	store := raffle.NewPostgres(pool)
	cands, err := store.Candidates(ctx, ev.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 5 {
		t.Fatalf("%d candidates (cancelled and waitlisted must stay out)", len(cands))
	}
	for _, c := range cands {
		if c.TicketID == ticketOf["ada"] && (len(c.Sessions) != 4 || !slices.Equal(c.Entered, []string{"2026-11-27"}) || c.Name != "ada Üye" || c.Email != "ada@example.com") {
			t.Fatalf("ada %+v", c)
		}
		if c.TicketID == ticketOf["edsger"] && (c.TicketType != raffle.TypeGuest || c.Email != "edsger@example.com" || c.OwnerID != nil) {
			t.Fatalf("edsger %+v", c)
		}
	}

	audit := teamaudit.NewPostgres(pool)
	lead := authz.Principal{ID: uuid.NewString(), Groups: []string{"/UYELER/YK"}}
	svc := &raffle.Service{
		Store: store, Events: event.NewPostgresStore(pool), Authz: authz.NewAuthorizer(authz.DefaultPolicy()),
		Attendance: certificate.NewPostgresStore(pool), People: users.Accounts,
	}
	three := 3
	preview, err := svc.Preview(ctx, lead, ev.ID, raffle.Criteria{MinCounted: &three, EnteredOn: "2026-11-27"})
	if err != nil {
		t.Fatal(err)
	}
	if preview.Eligible != 3 {
		t.Fatalf("preview %+v", preview)
	}
	// Two draws at once among the same three, each of one winner and no
	// alternate, both leaving out past winners: never the same winner.
	var wg sync.WaitGroup
	drawn := make([]raffle.Raffle, 2)
	errs := make([]error, 2)
	for i := range 2 {
		wg.Go(func() {
			drawn[i], errs[i] = svc.Draw(ctx, lead, ev.ID, raffle.Request{
				Criteria: raffle.Criteria{MinCounted: &three, EnteredOn: "2026-11-27", ExcludePastWinners: true}, Winners: 1,
			})
		})
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if drawn[0].Entries[0].TicketID == drawn[1].Entries[0].TicketID {
		t.Fatal("two draws at once share a winner")
	}
	if drawn[0].EligibleCount+drawn[1].EligibleCount != 5 {
		t.Fatalf("eligible %d and %d, want 3 and 2", drawn[0].EligibleCount, drawn[1].EligibleCount)
	}
	// Members only: 3 eligible, so of the 2 alternates asked 1 is drawn.
	r, err := svc.Draw(ctx, lead, ev.ID, raffle.Request{Title: "Kapanış", Criteria: raffle.Criteria{TicketType: raffle.TypeRegistered}, Winners: 2, Alternates: 2})
	if err != nil {
		t.Fatal(err)
	}
	if r.Title != "Kapanış" || r.EligibleCount != 3 || r.AlternateCount != 1 || len(r.Entries) != 3 || r.DrawnBy == nil || r.DrawnBy.ID.String() != lead.ID {
		t.Fatalf("raffle %+v", r)
	}
	v, err := svc.Verification(ctx, lead, ev.ID, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := raffle.Verify(v); err != nil {
		t.Fatal(err)
	}
	after, err := svc.Forfeit(ctx, lead, ev.ID, r.ID, 2, "Salonda değildi")
	if err != nil {
		t.Fatal(err)
	}
	if after.Entries[1].Status != raffle.StatusForfeited || after.Entries[1].ForfeitedAt == nil || *after.Entries[1].ForfeitReason != "Salonda değildi" ||
		after.Entries[2].Status != raffle.StatusWinner || after.Entries[2].PromotedAt == nil {
		t.Fatalf("after forfeit %+v", after.Entries)
	}
	if _, err := svc.Forfeit(ctx, lead, ev.ID, r.ID, 4, "Gelmedi"); !errors.Is(err, raffle.ErrNotWinner) {
		t.Fatalf("alternate forfeited: %v", err)
	}
	list, err := svc.List(ctx, lead, ev.ID)
	if err != nil || len(list) != 3 || list[0].ID != r.ID {
		t.Fatalf("list %v %d", err, len(list))
	}
	if _, err := store.Get(ctx, uuid.New(), r.ID); !errors.Is(err, raffle.ErrNotFound) {
		t.Fatalf("another event's raffle: %v", err)
	}

	// The Team audit record takes the raffle rows on an event editor's
	// basis (the migration's checks).
	for _, action := range []teamaudit.Action{teamaudit.ActionRaffleDraw, teamaudit.ActionRaffleForfeit} {
		id, err := audit.Begin(ctx, teamaudit.Record{
			Actor:  teamaudit.Actor{ID: uuid.New(), Basis: teamaudit.BasisEventEditor, ViaGroupID: uuid.NewString(), Client: "core-frontend"},
			Action: action, GroupID: uuid.NewString(), GroupPath: "/UYELER/YK",
			After: &teamaudit.State{EventID: ev.ID.String()},
		})
		if err != nil {
			t.Fatalf("%s: %v", action, err)
		}
		if err := audit.Finish(ctx, id, teamaudit.OutcomeOK, "", &teamaudit.State{EventID: ev.ID.String(), Raffle: &teamaudit.RaffleInfo{ID: r.ID.String(), Eligible: 5, Winners: 2}}); err != nil {
			t.Fatal(err)
		}
	}
	// Every action the code knows is one the table takes.
	for _, action := range teamaudit.Actions {
		var ok bool
		if err := pool.QueryRow(ctx, `SELECT pg_get_constraintdef(oid) LIKE '%''' || $1 || '''%' FROM pg_constraint WHERE conname = 'team_audit_records_action_check'`,
			string(action)).Scan(&ok); err != nil || !ok {
			t.Errorf("the action check leaves out %s (%v)", action, err)
		}
	}

	// Erasure forgets who drew and forfeited and the reason on the erased
	// holder's Ticket; the places stay.
	erase := func(id uuid.UUID) {
		t.Helper()
		if _, err := pool.Exec(ctx, `UPDATE users SET account_state = 'deletion_pending', deletion_requested_at = now(), updated_at = now() WHERE id = $1`, id); err != nil {
			t.Fatal(err)
		}
		if err := users.AnonymizeAccount(ctx, id, time.Now().UTC(), nil); err != nil {
			t.Fatal(err)
		}
	}
	var holder uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT owner_id FROM tickets WHERE id = (SELECT ticket_id FROM event_raffle_entries WHERE raffle_id = $1 AND position = 2)`, r.ID).Scan(&holder); err != nil {
		t.Fatal(err)
	}
	erase(holder)
	var reason *string
	if err := pool.QueryRow(ctx, `SELECT forfeit_reason FROM event_raffle_entries WHERE raffle_id = $1 AND position = 2`, r.ID).Scan(&reason); err != nil || reason != nil {
		t.Fatalf("reason kept after erasure: %v %v", err, reason)
	}
	drawer := uuid.MustParse(lead.ID)
	if _, _, err := users.Upsert(ctx, user.User{ID: drawer, Email: "lead@example.com", FirstName: "Lider", LastName: "Kişi"}); err != nil {
		t.Fatal(err)
	}
	erase(drawer)
	var drawnBy, forfeitedBy, promotedBy *uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT r.drawn_by, e.forfeited_by, p.promoted_by FROM event_raffles r
		JOIN event_raffle_entries e ON e.raffle_id = r.id AND e.position = 2
		JOIN event_raffle_entries p ON p.raffle_id = r.id AND p.position = 3
		WHERE r.id = $1`, r.ID).Scan(&drawnBy, &forfeitedBy, &promotedBy); err != nil {
		t.Fatal(err)
	}
	if drawnBy != nil || forfeitedBy != nil || promotedBy != nil {
		t.Fatalf("actors kept after erasure: %v %v %v", drawnBy, forfeitedBy, promotedBy)
	}
	if v2, err := svc.Verification(ctx, lead, ev.ID, r.ID); err != nil || raffle.Verify(v2) != nil {
		t.Fatalf("verification after erasure: %v", err)
	}

	// A guest's forfeit reason goes with their address: erasing the account
	// of that address clears it on the guest Ticket's place.
	g, err := svc.Draw(ctx, lead, ev.ID, raffle.Request{Criteria: raffle.Criteria{TicketType: raffle.TypeGuest}, Winners: 1, Alternates: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Forfeit(ctx, lead, ev.ID, g.ID, 1, "Salonda değildi"); err != nil {
		t.Fatal(err)
	}
	var guestEmail string
	if err := pool.QueryRow(ctx, `SELECT guest_email FROM tickets WHERE id = $1`, g.Entries[0].TicketID).Scan(&guestEmail); err != nil {
		t.Fatal(err)
	}
	guestAccount := uuid.New()
	if _, _, err := users.Upsert(ctx, user.User{ID: guestAccount, Email: strings.ToLower(guestEmail), FirstName: "Misafir", LastName: "Hesap"}); err != nil {
		t.Fatal(err)
	}
	erase(guestAccount)
	var guestReason *string
	if err := pool.QueryRow(ctx, `SELECT forfeit_reason FROM event_raffle_entries WHERE raffle_id = $1 AND position = 1`, g.ID).Scan(&guestReason); err != nil || guestReason != nil {
		t.Fatalf("guest reason kept after erasure: %v %v", err, guestReason)
	}
	var other *string
	if err := pool.QueryRow(ctx, `SELECT forfeit_reason FROM event_raffle_entries WHERE raffle_id = $1 AND position = 2`, g.ID).Scan(&other); err != nil || other != nil {
		t.Fatalf("the alternate's place: %v %v", err, other)
	}
}
