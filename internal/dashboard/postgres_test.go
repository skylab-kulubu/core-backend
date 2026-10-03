package dashboard_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/skylab-kulubu/core-backend/internal/dashboard"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
	"github.com/skylab-kulubu/core-backend/internal/testpostgres"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

func migratedPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool := testpostgres.Start(t)
	if err := migrate.Apply(context.Background(), pool); err != nil {
		t.Fatal(err)
	}
	return pool
}

func exec(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

func TestPostgresStoreAggregates(t *testing.T) {
	pool := migratedPool(t)
	ctx := context.Background()
	store := dashboard.NewPostgresStore(pool)

	member := uuid.New()
	exec(t, pool, `INSERT INTO users (id, email) VALUES ($1, 'ada@example.com')`, member)
	a, b, other := uuid.New(), uuid.New(), uuid.New()
	for _, id := range []uuid.UUID{a, b, other} {
		exec(t, pool, `INSERT INTO events (id, name, location, owner_team) VALUES ($1, 'E', 'L', 'WEBLAB')`, id)
	}
	day, session := uuid.New(), uuid.New()
	exec(t, pool, `INSERT INTO event_days (id, event_id) VALUES ($1, $2)`, day, a)
	exec(t, pool, `INSERT INTO sessions (id, event_day_id, title, session_type) VALUES ($1, $2, 'S', 'TALK')`, session, day)
	session2 := uuid.New()
	exec(t, pool, `INSERT INTO sessions (id, event_day_id, title, session_type) VALUES ($1, $2, 'S2', 'TALK')`, session2, day)

	istanbul, err := time.LoadLocation("Europe/Istanbul")
	if err != nil {
		t.Fatal(err)
	}
	// 2026-10-02 22:30 UTC is 2026-10-03 01:30 in Istanbul.
	lateNight := time.Date(2026, 10, 2, 22, 30, 0, 0, time.UTC)
	earlier := time.Date(2026, 9, 20, 9, 0, 0, 0, time.UTC)
	tooOld := time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC)
	ticket := func(eventID uuid.UUID, kind string, owner *uuid.UUID, guest string, created time.Time) uuid.UUID {
		id := uuid.New()
		exec(t, pool, `INSERT INTO tickets (id, event_id, ticket_type, owner_id, guest_email, created_at) VALUES ($1, $2, $3, $4, $5, $6)`,
			id, eventID, kind, owner, guest, created)
		return id
	}
	checkedTwice := ticket(a, "REGISTERED", &member, "", lateNight)
	ticket(a, "GUEST", nil, "g1@example.com", lateNight)
	checkedGuest := ticket(a, "GUEST", nil, "g2@example.com", earlier)
	// A member's Ticket whose owner was erased: detached, still REGISTERED.
	ticket(a, "REGISTERED", nil, "", tooOld)
	ticket(b, "GUEST", nil, "g3@example.com", earlier)
	ticket(other, "GUEST", nil, "g4@example.com", lateNight)
	exec(t, pool, `INSERT INTO ticket_checkins (id, ticket_id, event_day_id, session_id) VALUES ($1, $2, $3, $4)`, uuid.New(), checkedTwice, day, session)
	exec(t, pool, `INSERT INTO ticket_checkins (id, ticket_id, event_day_id, session_id) VALUES ($1, $2, $3, $4)`, uuid.New(), checkedTwice, day, session2)
	exec(t, pool, `INSERT INTO ticket_checkins (id, ticket_id, event_day_id, session_id) VALUES ($1, $2, $3, $4)`, uuid.New(), checkedGuest, day, session)
	// Archiving the session keeps its check-ins, as the applicant list does.
	exec(t, pool, `UPDATE sessions SET archived_at = now() WHERE id = $1`, session)

	counts, err := store.TicketCounts(ctx, []uuid.UUID{a, b})
	if err != nil {
		t.Fatal(err)
	}
	if len(counts) != 2 {
		t.Fatalf("counts %+v", counts)
	}
	if got := counts[a]; got != (dashboard.TicketCounts{Applications: 4, Members: 2, Guests: 2, CheckedIn: 2}) {
		t.Fatalf("event a %+v", got)
	}
	if got := counts[b]; got != (dashboard.TicketCounts{Applications: 1, Guests: 1}) {
		t.Fatalf("event b %+v", got)
	}

	since := time.Date(2026, 9, 3, 21, 0, 0, 0, time.UTC)
	daily, err := store.DailyApplications(ctx, []uuid.UUID{a, b}, since, istanbul)
	if err != nil {
		t.Fatal(err)
	}
	if daily[a]["2026-10-03"] != 2 || daily[a]["2026-09-20"] != 1 || len(daily[a]) != 2 {
		t.Fatalf("event a daily %v", daily[a])
	}
	if daily[b]["2026-09-20"] != 1 || len(daily[b]) != 1 {
		t.Fatalf("event b daily %v", daily[b])
	}
	if _, asked := daily[other]; asked {
		t.Fatal("an Event not asked about was counted")
	}

	empty, err := store.TicketCounts(ctx, nil)
	if err != nil || len(empty) != 0 {
		t.Fatalf("no Events: %v %v", empty, err)
	}
}

// accountsContract checks a Store's Accounts over people seeded the same
// way in each store: an active person with names, one whose erasure was
// requested, one anonymized, and one core has no row for.
func accountsContract(t *testing.T, store dashboard.Store, active, pending, anonymized uuid.UUID) {
	t.Helper()
	unknown := uuid.New()
	got, err := store.Accounts(context.Background(), []uuid.UUID{active, pending, anonymized, unknown})
	if err != nil {
		t.Fatal(err)
	}
	if a := got[active]; a.Blocked || !a.Stored || a.FirstName != "Ada" || a.LastName != "Lovelace" {
		t.Fatalf("active %+v", a)
	}
	if !got[pending].Blocked || !got[anonymized].Blocked {
		t.Fatalf("pending %+v anonymized %+v", got[pending], got[anonymized])
	}
	if _, listed := got[unknown]; listed || len(got) != 3 {
		t.Fatalf("accounts %+v", got)
	}
	if none, err := store.Accounts(context.Background(), nil); err != nil || len(none) != 0 {
		t.Fatalf("no people: %v %v", none, err)
	}
}

func TestPostgresStoreAccountsFollowErasure(t *testing.T) {
	pool := migratedPool(t)
	ctx := context.Background()
	store := dashboard.NewPostgresStore(pool)
	users := user.NewPostgresStore(pool)

	active, pending, anonymized := uuid.New(), uuid.New(), uuid.New()
	exec(t, pool, `INSERT INTO users (id, email, first_name, last_name) VALUES ($1, 'ada@example.com', 'Ada', 'Lovelace')`, active)
	for _, id := range []uuid.UUID{pending, anonymized} {
		exec(t, pool, `INSERT INTO users (id, email) VALUES ($1, $2)`, id, id.String()+"@example.com")
	}
	if _, err := users.RequestDeletion(ctx, pending, nil); err != nil {
		t.Fatal(err)
	}
	exec(t, pool, `UPDATE users SET account_state = 'anonymized' WHERE id = $1`, anonymized)
	accountsContract(t, store, active, pending, anonymized)

	// A deletion marker outlives the row it was for.
	purged := uuid.New()
	exec(t, pool, `INSERT INTO users (id) VALUES ($1)`, purged)
	if _, err := users.RequestDeletion(ctx, purged, nil); err != nil {
		t.Fatal(err)
	}
	exec(t, pool, `DELETE FROM users WHERE id = $1`, purged)
	got, err := store.Accounts(ctx, []uuid.UUID{purged})
	if err != nil || !got[purged].Blocked || got[purged].Stored {
		t.Fatalf("purged %+v %v", got[purged], err)
	}
}

// The in-memory store answers as the Postgres one does.
func TestMemoryStoreAccountsFollowErasure(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	users := user.NewMemoryStore()
	active, pending, anonymized := uuid.New(), uuid.New(), uuid.New()
	for _, u := range []user.User{
		{ID: active, Email: "ada@example.com", FirstName: "Ada", LastName: "Lovelace"},
		{ID: pending, Email: "p@example.com"},
		{ID: anonymized, Email: "a@example.com"},
	} {
		if _, _, err := users.Upsert(ctx, u); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []uuid.UUID{pending, anonymized} {
		if _, err := users.RequestDeletion(ctx, id, nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := users.AnonymizeAccount(ctx, anonymized, time.Now(), nil); err != nil {
		t.Fatal(err)
	}
	accountsContract(t, dashboard.NewMemoryStore(nil, users), active, pending, anonymized)
}
