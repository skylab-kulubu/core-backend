package certificate_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

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

// pgAuto is a migrated database with a leader account and a certificate
// service over it.
type pgAuto struct {
	pool     *pgxpool.Pool
	events   *event.PostgresStore
	svc      certificate.Service
	mail     *autoMail
	leaderID uuid.UUID
	leader   authz.Principal
}

func newPGAuto(t *testing.T) pgAuto {
	t.Helper()
	ctx := context.Background()
	pool := testpostgres.Start(t)
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	users := user.NewPostgresStore(pool)
	f := pgAuto{pool: pool, events: event.NewPostgresStore(pool), mail: &autoMail{}, leaderID: uuid.New()}
	if _, _, err := users.Upsert(ctx, user.User{ID: f.leaderID, Email: "lider@example.com", FirstName: "Lider", LastName: "ARTLAB"}); err != nil {
		t.Fatal(err)
	}
	f.leader = authz.Principal{ID: f.leaderID.String(), Groups: []string{"/UYELER/ARGE/ARTLAB/LIDERLER"}}
	f.svc = f.replica()
	return f
}

// replica is another core over the same database.
func (f pgAuto) replica() certificate.Service {
	store := certificate.NewPostgresStore(f.pool)
	return certificate.NewServiceWithOptions(store, ticket.NewPostgresStore(f.pool), f.events, user.NewPostgresStore(f.pool),
		authz.NewAuthorizer(authz.DefaultPolicy()), &recRender{}, f.mail, certificate.Options{PublicAPIOrigin: "https://api.example.test"})
}

// autoEvent is one Event the schedule may close.
type autoEvent struct {
	name   string
	rule   string
	status event.Status
	auto   bool
	// ends are its live Sessions' end times; none is an Event with no
	// Session.
	ends []time.Time
}

// create writes the Event (its end date two days after the last Session, so
// that only the Sessions' ends matter), its Sessions on one day and a Guest
// checked in on the first.
func (f pgAuto) create(t *testing.T, e autoEvent) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	anchor := time.Date(2031, 3, 14, 12, 0, 0, 0, time.UTC)
	if len(e.ends) > 0 {
		anchor = e.ends[len(e.ends)-1]
	}
	start, end := anchor.Add(-6*time.Hour), anchor.Add(48*time.Hour)
	status := e.status
	if status == event.StatusCancelled {
		status = event.StatusPublished
	}
	ev, err := f.events.Create(ctx, event.Event{Name: e.name, Location: "YTÜ", OwnerTeam: "ARTLAB", AttendanceRule: e.rule, AttendanceAutoFinalize: e.auto,
		Status: status, StartDate: &start, EndDate: &end, PublishedBy: &f.leaderID})
	if err != nil {
		t.Fatal(err)
	}
	if e.status == event.StatusCancelled {
		if _, err := f.pool.Exec(ctx, `UPDATE events SET status = 'cancelled', cancelled_at = now() WHERE id = $1`, ev.ID); err != nil {
			t.Fatal(err)
		}
	}
	if len(e.ends) == 0 {
		return ev.ID
	}
	dayID, guestID := uuid.New(), uuid.New()
	if _, err := f.pool.Exec(ctx, `INSERT INTO event_days (id, event_id, name) VALUES ($1, $2, 'Gün')`, dayID, ev.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `INSERT INTO tickets (id, event_id, ticket_type, guest_first_name, guest_last_name, guest_email) VALUES ($1, $2, 'GUEST', 'Ayşe', 'Yılmaz', 'ayse@example.com')`, guestID, ev.ID); err != nil {
		t.Fatal(err)
	}
	for i, ends := range e.ends {
		sessionID := uuid.New()
		if _, err := f.pool.Exec(ctx, `INSERT INTO sessions (id, event_day_id, title, session_type, order_index, certificate_counted, start_time, end_time)
			VALUES ($1, $2, 'Oturum', 'TALK', $3, true, $4, $5)`, sessionID, dayID, i, ends.Add(-time.Hour), ends); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			if _, err := f.pool.Exec(ctx, `INSERT INTO ticket_checkins (id, ticket_id, event_day_id, session_id) VALUES ($1, $2, $3, $4)`, uuid.New(), guestID, dayID, sessionID); err != nil {
				t.Fatal(err)
			}
		}
	}
	return ev.ID
}

// closed is the Event's finalization (source, by) and its batches
// (reason/requested_by/total), from the database.
func (f pgAuto) closed(t *testing.T, eventID uuid.UUID) (source string, by *uuid.UUID, batches []string) {
	t.Helper()
	ctx := context.Background()
	err := f.pool.QueryRow(ctx, `SELECT finalized_source, attendance_finalized_by FROM certificate_event_state WHERE event_id = $1`, eventID).Scan(&source, &by)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		t.Fatal(err)
	}
	rows, err := f.pool.Query(ctx, `SELECT reason || '/' || COALESCE(requested_by::text, 'system') || '/' || total_count FROM certificate_batches WHERE event_id = $1 ORDER BY created_at`, eventID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var b string
		if err := rows.Scan(&b); err != nil {
			t.Fatal(err)
		}
		batches = append(batches, b)
	}
	return source, by, batches
}

// The schedule on a fixed clock: an Event whose last live Session ends at
// 17:00 is not closed at 17:59 and is at 18:00, as the system (source auto,
// no finalizer, no requester), with its publisher told; one due more than
// the window ago, and a rule of none, a draft, a cancelled, an archived
// Event, one with the switch off and one with no Session never are. A second
// pass opens no second batch.
func TestPostgresAutoFinalizeSchedule(t *testing.T) {
	ctx := context.Background()
	f := newPGAuto(t)
	five := time.Date(2031, 3, 14, 17, 0, 0, 0, time.UTC)
	due := f.create(t, autoEvent{name: "Zamanında", rule: certificate.RuleOnce, status: event.StatusPublished, auto: true, ends: []time.Time{five.Add(-2 * time.Hour), five}})
	skipped := map[string]uuid.UUID{
		"window passed": f.create(t, autoEvent{name: "Eski", rule: certificate.RuleOnce, status: event.StatusPublished, auto: true, ends: []time.Time{five.Add(-7*24*time.Hour - time.Hour)}}),
		"rule none":     f.create(t, autoEvent{name: "Kuralsız", rule: certificate.RuleNone, status: event.StatusPublished, auto: true, ends: []time.Time{five}}),
		"draft":         f.create(t, autoEvent{name: "Taslak", rule: certificate.RuleOnce, status: event.StatusDraft, auto: true, ends: []time.Time{five}}),
		"cancelled":     f.create(t, autoEvent{name: "İptal", rule: certificate.RuleOnce, status: event.StatusCancelled, auto: true, ends: []time.Time{five}}),
		"archived":      f.create(t, autoEvent{name: "Arşiv", rule: certificate.RuleOnce, status: event.StatusPublished, auto: true, ends: []time.Time{five}}),
		"switch off":    f.create(t, autoEvent{name: "Kapalı", rule: certificate.RuleOnce, status: event.StatusPublished, auto: false, ends: []time.Time{five}}),
		"no Session":    f.create(t, autoEvent{name: "Oturumsuz", rule: certificate.RuleOnce, status: event.StatusPublished, auto: true}),
	}
	if _, err := f.pool.Exec(ctx, `UPDATE events SET archived_at = now() WHERE id = $1`, skipped["archived"]); err != nil {
		t.Fatal(err)
	}
	// The Event with no Session ended by its dates long before 18:00.
	if _, err := f.pool.Exec(ctx, `UPDATE events SET start_date = $2, end_date = $3 WHERE id = $1`, skipped["no Session"], five.Add(-3*time.Hour), five); err != nil {
		t.Fatal(err)
	}

	result, err := f.svc.AutoFinalize(ctx, five.Add(59*time.Minute))
	if err != nil || !result.Locked || result.Finalized != 0 || len(result.Errors) != 0 {
		t.Fatalf("17:59: %v %+v", err, result)
	}
	if source, _, batches := f.closed(t, due); source != "" || len(batches) != 0 {
		t.Fatalf("closed at 17:59: %q %v", source, batches)
	}
	result, err = f.svc.AutoFinalize(ctx, five.Add(time.Hour))
	if err != nil || result.Finalized != 1 || len(result.Errors) != 0 {
		t.Fatalf("18:00: %v %+v", err, result)
	}
	source, by, batches := f.closed(t, due)
	if source != certificate.FinalizedAuto || by != nil || strings.Join(batches, ",") != "finalization/system/1" {
		t.Fatalf("18:00: source %q by %v batches %v", source, by, batches)
	}
	if len(f.mail.sent) != 1 || f.mail.sent[0] != "lider@example.com" || f.mail.count[0] != "1" {
		t.Fatalf("publisher mails %v %v", f.mail.sent, f.mail.count)
	}
	for why, id := range skipped {
		if source, _, batches := f.closed(t, id); source != "" || len(batches) != 0 {
			t.Errorf("%s: closed (%q %v)", why, source, batches)
		}
	}
	result, err = f.svc.AutoFinalize(ctx, five.Add(time.Hour+time.Minute))
	if err != nil || result.Finalized != 0 {
		t.Fatalf("second pass: %v %+v", err, result)
	}
	if _, _, batches := f.closed(t, due); len(batches) != 1 {
		t.Fatalf("second pass opened another batch: %v", batches)
	}
	summary, err := f.svc.Summary(ctx, f.leader, due)
	if err != nil || summary.FinalizedSource != certificate.FinalizedAuto || summary.AttendanceFinalizedAt == nil || summary.AutoFinalizeAt != nil {
		t.Fatalf("summary %v %+v", err, summary)
	}
}

// Two replicas' passes at once close an Event once: one batch. So does a
// pass racing the organizer's manual finalization.
func TestPostgresAutoFinalizeRaces(t *testing.T) {
	ctx := context.Background()
	f := newPGAuto(t)
	other := f.replica()
	five := time.Date(2031, 3, 14, 17, 0, 0, 0, time.UTC)
	for round := range 5 {
		id := f.create(t, autoEvent{name: "Yarış", rule: certificate.RuleOnce, status: event.StatusPublished, auto: true, ends: []time.Time{five.Add(time.Duration(round) * time.Minute)}})
		at := five.Add(time.Duration(round)*time.Minute + time.Hour)
		var wg sync.WaitGroup
		results := make([]certificate.AutoFinalizeResult, 2)
		errs := make([]error, 2)
		for i, svc := range []certificate.Service{f.svc, other} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				results[i], errs[i] = svc.AutoFinalize(ctx, at)
			}()
		}
		wg.Wait()
		if errs[0] != nil || errs[1] != nil || results[0].Finalized+results[1].Finalized != 1 {
			t.Fatalf("round %d: %v %v %+v %+v", round, errs[0], errs[1], results[0], results[1])
		}
		if _, _, batches := f.closed(t, id); len(batches) != 1 {
			t.Fatalf("round %d: batches %v", round, batches)
		}
	}

	// The manual race needs an Event that has ended now (Finalize reads the
	// clock).
	now := time.Now().UTC().Truncate(time.Second)
	for round := range 5 {
		id := f.create(t, autoEvent{name: "Elle yarış", rule: certificate.RuleOnce, status: event.StatusPublished, auto: true, ends: []time.Time{now.Add(-2*time.Hour - time.Duration(round)*time.Minute)}})
		var wg sync.WaitGroup
		var manualErr, autoErr error
		var auto certificate.AutoFinalizeResult
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, manualErr = f.svc.Finalize(ctx, f.leader, id)
		}()
		go func() {
			defer wg.Done()
			auto, autoErr = other.AutoFinalize(ctx, now)
		}()
		wg.Wait()
		if autoErr != nil || len(auto.Errors) != 0 || (manualErr != nil && !errors.Is(manualErr, certificate.ErrConflict)) {
			t.Fatalf("round %d: manual %v auto %v %+v", round, manualErr, autoErr, auto)
		}
		if (manualErr == nil) == (auto.Finalized == 1) {
			t.Fatalf("round %d: manual %v, auto closed %d: want exactly one", round, manualErr, auto.Finalized)
		}
		source, _, batches := f.closed(t, id)
		if len(batches) != 1 || (manualErr == nil) != (source == certificate.FinalizedManual) {
			t.Fatalf("round %d: source %q batches %v manual %v", round, source, batches, manualErr)
		}
	}
}

// An Event whose template cannot be resolved (bound to a template with no
// published version) is skipped and counted; the other one closes; once
// the binding is gone the next pass closes the skipped one too.
func TestPostgresAutoFinalizeSkipsAFailingEvent(t *testing.T) {
	ctx := context.Background()
	f := newPGAuto(t)
	five := time.Date(2031, 3, 14, 17, 0, 0, 0, time.UTC)
	good := f.create(t, autoEvent{name: "İyi", rule: certificate.RuleOnce, status: event.StatusPublished, auto: true, ends: []time.Time{five}})
	broken := f.create(t, autoEvent{name: "Bozuk", rule: certificate.RuleOnce, status: event.StatusPublished, auto: true, ends: []time.Time{five}})
	templateID := uuid.New()
	if _, err := f.pool.Exec(ctx, `INSERT INTO certificate_templates (id, name, owner_team, source_kind) VALUES ($1, 'Yarım', 'ARTLAB', 'sky')`, templateID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `INSERT INTO certificate_template_bindings (id, scope, scope_key, template_id) VALUES ($1, 'event', $2, $3)`, uuid.New(), broken.String(), templateID); err != nil {
		t.Fatal(err)
	}
	result, err := f.svc.AutoFinalize(ctx, five.Add(time.Hour))
	if err != nil || result.Finalized != 1 || len(result.Errors) != 1 || !strings.Contains(result.Errors[0].Error(), broken.String()) {
		t.Fatalf("pass: %v %+v", err, result)
	}
	if source, _, _ := f.closed(t, good); source != certificate.FinalizedAuto {
		t.Fatalf("good: %q", source)
	}
	if source, _, batches := f.closed(t, broken); source != "" || len(batches) != 0 {
		t.Fatalf("broken closed: %q %v", source, batches)
	}
	if metrics := f.svc.(interface{ Prometheus() string }).Prometheus(); !strings.Contains(metrics, "core_certificate_auto_finalize_errors_total 1\n") {
		t.Fatalf("metrics %s", metrics)
	}
	if _, err := f.pool.Exec(ctx, `DELETE FROM certificate_template_bindings WHERE scope = 'event' AND scope_key = $1`, broken.String()); err != nil {
		t.Fatal(err)
	}
	result, err = f.svc.AutoFinalize(ctx, five.Add(time.Hour+time.Minute))
	if err != nil || result.Finalized != 1 || len(result.Errors) != 0 {
		t.Fatalf("next pass: %v %+v", err, result)
	}
	if source, _, batches := f.closed(t, broken); source != certificate.FinalizedAuto || len(batches) != 1 {
		t.Fatalf("broken after the fix: %q %v", source, batches)
	}
}

// An Event with no live Session closes an hour after its own end once
// someone entered it, and certifies who did (docs/certificate-rule.md, "An
// Event with no Session"); its certificates carry no attendance snapshot.
// One nobody entered is left alone (TestPostgresAutoFinalizeSchedule).
func TestPostgresAutoFinalizeSessionless(t *testing.T) {
	ctx := context.Background()
	f := newPGAuto(t)
	five := time.Date(2031, 3, 14, 17, 0, 0, 0, time.UTC)
	id := f.create(t, autoEvent{name: "Buluşma", rule: certificate.RuleCount, status: event.StatusPublished, auto: true})
	if _, err := f.pool.Exec(ctx, `UPDATE events SET start_date = $2, end_date = $3 WHERE id = $1`, id, five.Add(-3*time.Hour), five); err != nil {
		t.Fatal(err)
	}
	entered, absent := uuid.New(), uuid.New()
	for i, ticketID := range []uuid.UUID{entered, absent} {
		if _, err := f.pool.Exec(ctx, `INSERT INTO tickets (id, event_id, ticket_type, guest_first_name, guest_last_name, guest_email) VALUES ($1, $2, 'GUEST', 'Ayşe', 'Yılmaz', $3)`, ticketID, id, fmt.Sprintf("ayse%d@example.com", i)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.pool.Exec(ctx, `INSERT INTO venue_entries (id, ticket_id, event_id, entry_date, source) VALUES ($1, $2, $3, '2031-03-14', 'scan')`, uuid.New(), entered, id); err != nil {
		t.Fatal(err)
	}
	attendance, err := certificate.NewPostgresStore(f.pool).EventAttendance(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if !attendance.Sessionless || attendance.CountedTotal != 1 || attendance.LastSessionEnd == nil || !attendance.LastSessionEnd.Equal(five) {
		t.Fatalf("attendance %+v", attendance)
	}
	result, err := f.svc.AutoFinalize(ctx, five.Add(59*time.Minute))
	if err != nil || result.Finalized != 0 {
		t.Fatalf("17:59: %v %+v", err, result)
	}
	result, err = f.svc.AutoFinalize(ctx, five.Add(time.Hour))
	if err != nil || result.Finalized != 1 || len(result.Errors) != 0 {
		t.Fatalf("18:00: %v %+v", err, result)
	}
	if source, _, batches := f.closed(t, id); source != certificate.FinalizedAuto || strings.Join(batches, ",") != "finalization/system/1" {
		t.Fatalf("closed %q %v", source, batches)
	}
}
