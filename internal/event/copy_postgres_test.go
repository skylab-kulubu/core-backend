package event_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/competition"
	"github.com/skylab-kulubu/core-backend/internal/event"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
	"github.com/skylab-kulubu/core-backend/internal/testpostgres"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// copyDatabase is a migrated database with a source Event that has a day,
// Sessions, door staff, an Event template binding and Competitions.
type copyDatabase struct {
	pool     *pgxpool.Pool
	store    *event.PostgresStore
	svc      event.Service
	source   event.Event
	yk       authz.Principal
	staff    uuid.UUID
	template uuid.UUID
}

func newCopyDatabase(t *testing.T) copyDatabase {
	t.Helper()
	pool := testpostgres.Start(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	users := user.NewService(user.NewPostgresStore(pool))
	actor, staff := uuid.New(), uuid.New()
	for id, email := range map[uuid.UUID]string{actor: "yk@example.test", staff: "kapi@example.test"} {
		if _, _, err := users.Ensure(ctx, id, user.Profile{Email: email}); err != nil {
			t.Fatal(err)
		}
	}
	store := event.NewPostgresStore(pool).CopyCompetitionsWith(competition.CopyForEvent)
	d := copyDatabase{
		pool: pool, store: store,
		svc:   event.NewService(store, authz.NewAuthorizer(authz.DefaultPolicy())),
		yk:    authz.Principal{ID: actor.String(), Groups: []string{"/UYELER/YK"}},
		staff: staff,
	}
	var err error
	d.source, err = store.Create(ctx, event.Event{
		Name: "Gecekodu 2026", Location: "YTÜ", OwnerTeam: "WEBLAB",
		StartDate: stamp("2026-03-14T19:00:00+03:00"), EndDate: stamp("2026-03-15T03:00:00+03:00"),
		DoorStaffIDs: []uuid.UUID{staff},
	})
	if err != nil {
		t.Fatal(err)
	}
	day, err := store.CreateDay(ctx, event.Day{EventID: d.source.ID, Name: "Cumartesi", StartDate: stamp("2026-03-14T19:00:00+03:00")})
	if err != nil {
		t.Fatal(err)
	}
	for _, title := range []string{"Açılış", "Kapanış"} {
		created, err := store.CreateSession(ctx, event.Session{EventDayID: day.ID, Title: title, SpeakerName: "Ada", SessionType: "talk",
			StartTime: stamp("2026-03-14T20:00:00+03:00"), EndTime: stamp("2026-03-14T21:00:00+03:00"), Cancelled: true})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `UPDATE sessions SET cancelled = true WHERE id = $1`, created.ID); err != nil {
			t.Fatal(err)
		}
	}
	d.template = uuid.New()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO certificate_templates (id, name, owner_team) VALUES ($1, 'Gecekodu', 'WEBLAB')`, d.template)
	exec(`INSERT INTO certificate_template_bindings (id, scope, scope_key, template_id) VALUES ($1, 'event', $2, $3)`, uuid.New(), d.source.ID.String(), d.template)
	// A Competition with two stages, an entrant and a score; a cancelled
	// one; an archived one.
	running, archived := uuid.New(), uuid.New()
	exec(`INSERT INTO competitions (id, event_id, name, description, entrant_mode, status) VALUES ($1, $2, 'CTF', 'Bayrak yakala', 'individual', 'running')`, running, d.source.ID)
	exec(`INSERT INTO competitions (id, event_id, name, status, cancel_reason) VALUES ($1, $2, 'İptal', 'cancelled', 'Gerekçe')`, uuid.New(), d.source.ID)
	exec(`INSERT INTO competitions (id, event_id, name, archived_at) VALUES ($1, $2, 'Arşiv', now())`, archived, d.source.ID)
	stage := uuid.New()
	exec(`INSERT INTO competition_stages (id, competition_id, position, name, strategy, config, config_version, starts_at, ends_at)
		VALUES ($1, $2, 1, 'Ön eleme', 'manual', '{"max": 10}', 2, $3, $4)`, stage, running, stamp("2026-03-14T20:00:00+03:00"), stamp("2026-03-14T22:00:00+03:00"))
	exec(`INSERT INTO competition_stages (id, competition_id, position, name, strategy) VALUES ($1, $2, 2, 'Final', 'manual')`, uuid.New(), running)
	entrant := uuid.New()
	exec(`INSERT INTO competition_entrants (id, competition_id) VALUES ($1, $2)`, entrant, running)
	exec(`INSERT INTO competition_submissions (id, competition_id, stage_id, entrant_id, source, value) VALUES ($1, $2, $3, $4, 'manual', 5000)`, uuid.New(), running, stage, entrant)
	return d
}

func (d copyDatabase) count(t *testing.T, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := d.pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// One transaction writes the Event, its door staff, days, Sessions, template
// binding and Competitions; the Season is the narrowest current one the
// new start falls in.
func TestPostgresCopy(t *testing.T) {
	d := newCopyDatabase(t)
	ctx := context.Background()
	seasons := map[string]uuid.UUID{}
	for _, s := range []struct {
		name       string
		start, end *time.Time
		archived   bool
	}{
		{"geniş", stamp("2026-09-01T00:00:00+03:00"), stamp("2028-06-30T00:00:00+03:00"), false},
		{"dar", stamp("2026-09-01T00:00:00+03:00"), stamp("2027-06-30T00:00:00+03:00"), false},
		{"arşiv", stamp("2027-03-01T00:00:00+03:00"), stamp("2027-03-31T00:00:00+03:00"), true},
		{"sonsuz", stamp("2027-01-01T00:00:00+03:00"), nil, false},
	} {
		id := uuid.New()
		seasons[s.name] = id
		if _, err := d.pool.Exec(ctx, `INSERT INTO seasons (id, name, start_date, end_date, archived_at) VALUES ($1, $2, $3, $4, CASE WHEN $5 THEN now() END)`,
			id, s.name, s.start, s.end, s.archived); err != nil {
			t.Fatal(err)
		}
	}
	in := copyIn("Gecekodu 2027", "2027-03-13T19:00:00+03:00")
	in.IncludeDoorStaff, in.IncludeCompetitions = true, true
	got, fresh, err := d.svc.Copy(ctx, d.yk, d.source.ID, in)
	if err != nil || !fresh || len(got.Skipped) != 0 {
		t.Fatalf("copy: %+v %v %v", got.Skipped, fresh, err)
	}
	c := got.Event
	if c.Status != event.StatusDraft || c.Active || c.CopiedFromEventID == nil || *c.CopiedFromEventID != d.source.ID ||
		c.SeasonID == nil || *c.SeasonID != seasons["dar"] || !c.EndDate.Equal(*stamp("2027-03-14T03:00:00+03:00")) ||
		!slices.Equal(c.DoorStaffIDs, []uuid.UUID{d.staff}) {
		t.Fatalf("copy %+v", c)
	}
	var active bool
	if err := d.pool.QueryRow(ctx, `SELECT active FROM events WHERE id = $1`, c.ID).Scan(&active); err != nil || active {
		t.Fatalf("legacy active %v %v", active, err)
	}
	if n := d.count(t, `SELECT count(*) FROM sessions s JOIN event_days d ON d.id = s.event_day_id
		WHERE d.event_id = $1 AND NOT s.cancelled AND s.start_time = $2`, c.ID, stamp("2027-03-13T20:00:00+03:00")); n != 2 {
		t.Fatalf("sessions %d", n)
	}
	if n := d.count(t, `SELECT count(*) FROM certificate_template_bindings WHERE scope = 'event' AND scope_key = $1 AND template_id = $2`, c.ID.String(), d.template); n != 1 {
		t.Fatalf("binding %d", n)
	}
	var name, description, mode, status string
	var createdBy *uuid.UUID
	if err := d.pool.QueryRow(ctx, `SELECT name, description, entrant_mode, status, created_by FROM competitions WHERE event_id = $1 AND name = 'CTF'`, c.ID).
		Scan(&name, &description, &mode, &status, &createdBy); err != nil {
		t.Fatal(err)
	}
	if description != "Bayrak yakala" || mode != "individual" || status != "draft" || createdBy == nil || createdBy.String() != d.yk.ID {
		t.Fatalf("competition %s %s %s %v", description, mode, status, createdBy)
	}
	// The cancelled one comes back a draft without its reason; the archived
	// one stays behind.
	if n := d.count(t, `SELECT count(*) FROM competitions WHERE event_id = $1 AND status = 'draft' AND cancel_reason = '' AND archived_at IS NULL`, c.ID); n != 2 {
		t.Fatalf("competitions %d", n)
	}
	if n := d.count(t, `SELECT count(*) FROM competitions WHERE event_id = $1 AND name = 'Arşiv'`, c.ID); n != 0 {
		t.Fatal("archived competition copied")
	}
	if n := d.count(t, `SELECT count(*) FROM competition_stages st JOIN competitions c ON c.id = st.competition_id
		WHERE c.event_id = $1 AND c.name = 'CTF' AND (
			(st.position = 1 AND st.name = 'Ön eleme' AND st.config = '{"max": 10}' AND st.config_version = 2
				AND st.starts_at = $2 AND st.ends_at = $3)
			OR (st.position = 2 AND st.starts_at IS NULL))`, c.ID, stamp("2027-03-13T20:00:00+03:00"), stamp("2027-03-13T22:00:00+03:00")); n != 2 {
		t.Fatalf("stages %d", n)
	}
	for _, table := range []string{"competition_entrants", "competition_submissions"} {
		if n := d.count(t, `SELECT count(*) FROM `+table+` x JOIN competitions c ON c.id = x.competition_id WHERE c.event_id = $1`, c.ID); n != 0 {
			t.Fatalf("%s copied: %d", table, n)
		}
	}
	// Retried: the same copy, nothing written twice.
	again, fresh, err := d.svc.Copy(ctx, d.yk, d.source.ID, in)
	if err != nil || fresh || again.Event.ID != c.ID || d.count(t, `SELECT count(*) FROM events WHERE copied_from_event_id = $1`, d.source.ID) != 1 {
		t.Fatalf("again %v %v", fresh, err)
	}
	if _, _, err := d.store.Copy(ctx, event.CopyPlan{Source: d.source.ID, Event: event.Event{ID: c.ID, Name: "x", OwnerTeam: "WEBLAB", StartDate: stamp("2027-01-01T00:00:00Z")}}); !errors.Is(err, event.ErrConflict) {
		t.Fatalf("store, id taken: %v", err)
	}
}

// What the copier may not write, or the template's team refuses, is left
// out and named; the copy is made.
func TestPostgresCopySkips(t *testing.T) {
	d := newCopyDatabase(t)
	ctx := context.Background()
	leader := authz.Principal{ID: uuid.NewString(), Groups: []string{"/UYELER/ARGE/WEBLAB/LIDERLER"}}
	in := copyIn("Kopya", "2027-03-13T19:00:00+03:00")
	in.IncludeDoorStaff, in.IncludeCompetitions = true, true
	// A WEBLAB Leader binds templates and manages Competitions, but does
	// not assign door staff (event:manage).
	got, _, err := d.svc.Copy(ctx, leader, d.source.ID, in)
	if err != nil || !slices.Equal(got.Skipped, []string{event.SkippedDoorStaff}) {
		t.Fatalf("leader: %v %v", got.Skipped, err)
	}
	// Another team's template does not serve WEBLAB.
	if _, err := d.pool.Exec(ctx, `UPDATE certificate_templates SET owner_team = 'ARTLAB' WHERE id = $1`, d.template); err != nil {
		t.Fatal(err)
	}
	got, _, err = d.svc.Copy(ctx, d.yk, d.source.ID, copyIn("Kopya 2", "2027-03-13T19:00:00+03:00"))
	if err != nil || !slices.Equal(got.Skipped, []string{event.SkippedCertificateTemplate}) {
		t.Fatalf("template: %v %v", got.Skipped, err)
	}
	if n := d.count(t, `SELECT count(*) FROM certificate_template_bindings WHERE scope_key = $1`, got.Event.ID.String()); n != 0 {
		t.Fatalf("binding written: %d", n)
	}
	// Not asked for, Competitions are not copied and not named.
	if n := d.count(t, `SELECT count(*) FROM competitions WHERE event_id = $1`, got.Event.ID); n != 0 {
		t.Fatalf("competitions %d", n)
	}
}

// A failure halfway leaves nothing behind: no Event, no day, no Session.
func TestPostgresCopyIsOneTransaction(t *testing.T) {
	d := newCopyDatabase(t)
	ctx := context.Background()
	if _, err := d.pool.Exec(ctx, `
		CREATE FUNCTION refuse_closing() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			IF NEW.title = 'Kapanış' THEN RAISE EXCEPTION 'refused'; END IF;
			RETURN NEW;
		END $$;
		CREATE TRIGGER refuse_closing BEFORE INSERT ON sessions FOR EACH ROW EXECUTE FUNCTION refuse_closing();`); err != nil {
		t.Fatal(err)
	}
	in := copyIn("Kopya", "2027-03-13T19:00:00+03:00")
	in.IncludeDoorStaff, in.IncludeCompetitions = true, true
	if _, _, err := d.svc.Copy(ctx, d.yk, d.source.ID, in); err == nil {
		t.Fatal("copy passed the refusal")
	}
	for _, sql := range []string{
		`SELECT count(*) FROM events WHERE id = $1`,
		`SELECT count(*) FROM event_days WHERE event_id = $1`,
		`SELECT count(*) FROM event_door_staff WHERE event_id = $1`,
	} {
		if n := d.count(t, sql, in.ID); n != 0 {
			t.Fatalf("%s: %d", sql, n)
		}
	}
	if n := d.count(t, `SELECT count(*) FROM sessions WHERE title = 'Açılış'`); n != 1 {
		t.Fatalf("sessions %d", n)
	}
}
