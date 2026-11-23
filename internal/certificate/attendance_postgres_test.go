package certificate_test

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/certificate"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
	"github.com/skylab-kulubu/core-backend/internal/testpostgres"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// attendanceQueries counts the queries core sends to Postgres.
type attendanceQueries struct{ n atomic.Int64 }

func (c *attendanceQueries) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	c.n.Add(1)
	return ctx
}

func (c *attendanceQueries) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// pgProgram writes a count Event of 3 days and 10 Sessions:
//
//	day 1: S0 Açılış (required), S1, S2, S3
//	day 2: S4 Ağ (not counted), S5, S6
//	day 3: S7, S8 Final (required, cancelled), S9
//
// N = 8 (S0–S3, S5–S7, S9), the default threshold ⌈3·8/4⌉ = 6.
func pgProgram(t *testing.T, ctx context.Context, pool *pgxpool.Pool, name string) (event.Event, []uuid.UUID, []uuid.UUID) {
	t.Helper()
	ev, err := event.NewPostgresStore(pool).Create(ctx, event.Event{Name: name, Location: "YTÜ", OwnerTeam: "ARTLAB", AttendanceRule: certificate.RuleCount})
	if err != nil {
		t.Fatal(err)
	}
	days := []uuid.UUID{uuid.New(), uuid.New(), uuid.New()}
	for i, day := range days {
		if _, err := pool.Exec(ctx, `INSERT INTO event_days (id, event_id, name, start_date) VALUES ($1, $2, $3, now() + make_interval(days => $4))`, day, ev.ID, "Gün", i); err != nil {
			t.Fatal(err)
		}
	}
	sessions := make([]uuid.UUID, 10)
	for i := range sessions {
		sessions[i] = uuid.New()
		day := days[0]
		switch {
		case i >= 7:
			day = days[2]
		case i >= 4:
			day = days[1]
		}
		title, kind, counted, required, cancelled := "Talk", "TALK", true, false, false
		switch i {
		case 0:
			title, required = "Açılış", true
		case 4:
			title, kind, counted = "Ağ", "NETWORKING", false
		case 8:
			title, required, cancelled = "Final", true, true
		}
		if _, err := pool.Exec(ctx, `INSERT INTO sessions (id, event_day_id, title, session_type, order_index, certificate_counted, certificate_required, cancelled)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`, sessions[i], day, title, kind, i, counted, required, cancelled); err != nil {
			t.Fatal(err)
		}
	}
	dayOf := make([]uuid.UUID, 10)
	for i := range dayOf {
		switch {
		case i >= 7:
			dayOf[i] = days[2]
		case i >= 4:
			dayOf[i] = days[1]
		default:
			dayOf[i] = days[0]
		}
	}
	return ev, sessions, dayOf
}

// The list and the Summary read an Event of 200 Tickets with as many queries
// as one of 7, and the store answers the same rows as the memory one: the
// NETWORKING check-in is not counted, the cancelled required Session is not
// missing, and the status follows issued > queued > failed > revoked >
// eligible > below. Finalization queues by the marks; the manual issue keeps
// its justification.
func TestPostgresAttendanceInFixedQueries(t *testing.T) {
	ctx := context.Background()
	plain := testpostgres.Start(t)
	if err := migrate.Apply(ctx, plain); err != nil {
		t.Fatal(err)
	}
	config := plain.Config()
	queries := &attendanceQueries{}
	config.ConnConfig.Tracer = queries
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	store := certificate.NewPostgresStore(pool)
	users := user.NewPostgresStore(pool)
	svc := certificate.NewServiceWithOptions(store, ticket.NewPostgresStore(pool), event.NewPostgresStore(pool), users,
		authz.NewAuthorizer(authz.DefaultPolicy()), &recRender{}, &recMail{}, certificate.Options{PublicAPIOrigin: "https://api.example.test"})
	// The batch names who asked: an active account.
	leaderID := uuid.New()
	if _, _, err := users.Upsert(ctx, user.User{ID: leaderID, Email: "lider@example.com", FirstName: "Lider", LastName: "ARTLAB"}); err != nil {
		t.Fatal(err)
	}
	leader := authz.Principal{ID: leaderID.String(), Groups: []string{"/UYELER/ARGE/ARTLAB/LIDERLER"}}

	// The small Event: seven people, one of each status.
	small, sessions, dayOf := pgProgram(t, ctx, pool, "Küçük")
	person := func(first, last string, guest bool, attended ...int) uuid.UUID {
		t.Helper()
		id := uuid.New()
		if guest {
			if _, err := pool.Exec(ctx, `INSERT INTO tickets (id, event_id, ticket_type, guest_first_name, guest_last_name, guest_email) VALUES ($1, $2, 'GUEST', $3, $4, $5)`,
				id, small.ID, first, last, strings.ToLower(first)+"@example.com"); err != nil {
				t.Fatal(err)
			}
		} else {
			owner := uuid.New()
			if _, _, err := users.Upsert(ctx, user.User{ID: owner, Email: strings.ToLower(first) + "@example.com", FirstName: first, LastName: last}); err != nil {
				t.Fatal(err)
			}
			if _, err := pool.Exec(ctx, `INSERT INTO tickets (id, event_id, ticket_type, owner_id) VALUES ($1, $2, 'REGISTERED', $3)`, id, small.ID, owner); err != nil {
				t.Fatal(err)
			}
		}
		for _, i := range attended {
			if _, err := pool.Exec(ctx, `INSERT INTO ticket_checkins (id, ticket_id, event_day_id, session_id) VALUES ($1, $2, $3, $4)`, uuid.New(), id, dayOf[i], sessions[i]); err != nil {
				t.Fatal(err)
			}
		}
		return id
	}
	ayse := person("Ayşe", "Yılmaz", false, 0, 1, 2, 3, 5, 6)
	burak := person("Burak", "Kaya", false, 1, 2, 3, 4, 5, 6, 7)
	cem := person("Cem", "Demir", true, 0, 4, 8)
	deniz := person("Deniz", "Ak", true, 0, 1, 2, 3, 5, 6, 7, 9)
	ece := person("Ece", "Su", true, 0, 1, 2, 3, 5, 6)
	fatma := person("Fatma", "Er", true)
	gul := person("Gül", "Ay", true)

	batchID := uuid.New()
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO certificates (id, event_id, ticket_id, serial, recipient_name, recipient_email, event_name, owner_team, verify_url)
			VALUES ($1, $2, $3, 'DENIZ', 'Deniz Ak', 'deniz@example.com', 'Küçük', 'ARTLAB', '')`, []any{uuid.New(), small.ID, deniz}},
		{`INSERT INTO certificates (id, event_id, ticket_id, serial, recipient_name, recipient_email, event_name, owner_team, verify_url, revoked_at)
			VALUES ($1, $2, $3, 'ECE', 'Ece Su', 'ece@example.com', 'Küçük', 'ARTLAB', '', now())`, []any{uuid.New(), small.ID, ece}},
		{`INSERT INTO certificate_batches (id, event_id, template_version_id, template_source, reason)
			VALUES ($1, $2, '89c0bfe1-5be5-4ddb-8686-4865f51e3401', 'clubDefault', 'manual')`, []any{batchID, small.ID}},
		// Deniz: an issued certificate wins over a queued job.
		{`INSERT INTO certificate_jobs (id, batch_id, event_id, ticket_id, status) VALUES ($1, $2, $3, $4, 'queued')`, []any{uuid.New(), batchID, small.ID, deniz}},
		{`INSERT INTO certificate_jobs (id, batch_id, event_id, ticket_id, status) VALUES ($1, $2, $3, $4, 'failed')`, []any{uuid.New(), batchID, small.ID, fatma}},
		// Gül: an older failed job, then a queued one.
		{`INSERT INTO certificate_jobs (id, batch_id, event_id, ticket_id, status, created_at) VALUES ($1, $2, $3, $4, 'failed', now() - interval '1 hour')`, []any{uuid.New(), batchID, small.ID, gul}},
	} {
		if _, err := pool.Exec(ctx, q.sql, q.args...); err != nil {
			t.Fatal(err)
		}
	}
	otherBatch := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO certificate_batches (id, event_id, template_version_id, template_source, reason)
		VALUES ($1, $2, '89c0bfe1-5be5-4ddb-8686-4865f51e3401', 'clubDefault', 'manual')`, otherBatch, small.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO certificate_jobs (id, batch_id, event_id, ticket_id, status) VALUES ($1, $2, $3, $4, 'running')`, uuid.New(), otherBatch, small.ID, gul); err != nil {
		t.Fatal(err)
	}

	// The large Event: 200 Guests, each checked in on every Session.
	large, _, _ := pgProgram(t, ctx, pool, "Büyük")
	if _, err := pool.Exec(ctx, `INSERT INTO tickets (id, event_id, ticket_type, guest_first_name, guest_last_name, guest_email)
		SELECT gen_random_uuid(), $1, 'GUEST', 'Misafir', g::text, 'misafir' || g || '@example.com' FROM generate_series(1, 200) g`, large.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO ticket_checkins (id, ticket_id, event_day_id, session_id)
		SELECT gen_random_uuid(), t.id, s.event_day_id, s.id
		FROM tickets t JOIN event_days d ON d.event_id = t.event_id JOIN sessions s ON s.event_day_id = d.id
		WHERE t.event_id = $1`, large.ID); err != nil {
		t.Fatal(err)
	}

	count := func(read func(uuid.UUID) error, eventID uuid.UUID) int64 {
		t.Helper()
		queries.n.Store(0)
		if err := read(eventID); err != nil {
			t.Fatal(err)
		}
		return queries.n.Load()
	}
	summary := func(id uuid.UUID) error { _, err := svc.Summary(ctx, leader, id); return err }
	list := func(id uuid.UUID) error { _, err := svc.Attendance(ctx, leader, id); return err }
	if s, l := count(summary, small.ID), count(summary, large.ID); s != l {
		t.Fatalf("Summary: %d queries for 7 Tickets, %d for 200", s, l)
	} else {
		t.Logf("Summary: %d queries for 7 and for 200 Tickets", s)
	}
	if s, l := count(list, small.ID), count(list, large.ID); s != l {
		t.Fatalf("attendance list: %d queries for 7 Tickets, %d for 200", s, l)
	} else {
		t.Logf("attendance list: %d queries for 7 and for 200 Tickets", s)
	}

	got, err := svc.Attendance(ctx, leader, large.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.CountedTotal != 8 || got.EffectiveMin != 6 || len(got.Rows) != 200 || got.Rows[0].Attended != 8 || got.Rows[0].Status != certificate.StatusEligible {
		t.Fatalf("large: N=%d min=%d rows=%d first=%+v", got.CountedTotal, got.EffectiveMin, len(got.Rows), got.Rows[0])
	}

	got, err = svc.Attendance(ctx, leader, small.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.CountedTotal != 8 || got.EffectiveMin != 6 || len(got.Required) != 1 || got.Required[0].Title != "Açılış" {
		t.Fatalf("small header %+v", got)
	}
	rows := rowsByTicket(got)
	for _, want := range []struct {
		id       uuid.UUID
		name     string
		attended int
		missing  string
		status   string
	}{
		{ayse, "Ayşe Yılmaz", 6, "", certificate.StatusEligible},
		{burak, "Burak Kaya", 6, "Açılış", certificate.StatusBelow},
		{cem, "Cem Demir", 1, "", certificate.StatusBelow},
		{deniz, "Deniz Ak", 8, "", certificate.StatusIssued},
		{ece, "Ece Su", 6, "", certificate.StatusRevoked},
		{fatma, "Fatma Er", 0, "Açılış", certificate.StatusFailed},
		{gul, "Gül Ay", 0, "Açılış", certificate.StatusQueued},
	} {
		row := rows[want.id]
		titles := []string{}
		for _, m := range row.MissingRequired {
			titles = append(titles, m.Title)
		}
		if row.Name != want.name || row.Attended != want.attended || strings.Join(titles, ",") != want.missing || row.Status != want.status {
			t.Errorf("%s: %+v, want attended=%d missing=%q status=%s", want.name, row, want.attended, want.missing, want.status)
		}
	}
	if c := rows[deniz].Certificate; c == nil || c.Serial != "DENIZ" || c.RevokedAt != nil {
		t.Fatalf("deniz certificate %+v", c)
	}
	if c := rows[ece].Certificate; c == nil || c.Serial != "ECE" || c.RevokedAt == nil {
		t.Fatalf("ece certificate %+v", c)
	}
	sum, err := svc.Summary(ctx, leader, small.ID)
	if err != nil {
		t.Fatal(err)
	}
	if sum.CountedTotal != 8 || sum.EffectiveMin != 6 || sum.EligibleCount != 3 || sum.BelowCount != 2 {
		t.Fatalf("small summary %+v", sum)
	}

	// Finalization queues Ayşe and Ece (eligible, no active certificate; as
	// before, a revoked one does not hold it back), not Deniz (issued) nor
	// Burak (misses Açılış).
	batch, err := svc.Finalize(ctx, leader, small.ID)
	if err != nil {
		t.Fatal(err)
	}
	queuedTickets := map[uuid.UUID]bool{}
	for _, job := range batch.Jobs {
		queuedTickets[job.TicketID] = true
	}
	if batch.TotalCount != 2 || !queuedTickets[ayse] || !queuedTickets[ece] {
		t.Fatalf("finalization %+v", batch)
	}

	if _, err := svc.QueueManual(ctx, leader, small.ID, burak, ""); err == nil {
		t.Fatal("Burak queued without a justification")
	}
	manual, err := svc.QueueManual(ctx, leader, small.ID, burak, "Açılışa raporlu")
	if err != nil {
		t.Fatal(err)
	}
	batches, err := svc.ListBatches(ctx, leader, small.ID)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, b := range batches {
		if b.ID == manual.ID {
			found = b.Justification != nil && *b.Justification == "Açılışa raporlu"
		}
	}
	if !found {
		t.Fatalf("the manual batch lost its justification: %+v", batches)
	}
}
