package membershipterm_test

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/identity"
	"github.com/skylab-kulubu/core-backend/internal/membershipterm"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
	"github.com/skylab-kulubu/core-backend/internal/season"
	"github.com/skylab-kulubu/core-backend/internal/teamaudit"
	"github.com/skylab-kulubu/core-backend/internal/teammail"
	"github.com/skylab-kulubu/core-backend/internal/testpostgres"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// The club of these tests: ADMIN, YK, DK, the ARGE area, its Team WEBLAB
// with its Leader group, and WEBLAB's Sub-team AI with its own.
var (
	gUyeler  = uuid.MustParse("00000000-0000-0000-0000-0000000000a0")
	gAdmin   = uuid.MustParse("00000000-0000-0000-0000-0000000000a1")
	gYK      = uuid.MustParse("00000000-0000-0000-0000-0000000000a2")
	gDK      = uuid.MustParse("00000000-0000-0000-0000-0000000000a3")
	gArge    = uuid.MustParse("00000000-0000-0000-0000-0000000000a4")
	gWeblab  = uuid.MustParse("00000000-0000-0000-0000-0000000000a5")
	gWeblabL = uuid.MustParse("00000000-0000-0000-0000-0000000000a6")
	gAI      = uuid.MustParse("00000000-0000-0000-0000-0000000000a7")
	gAIL     = uuid.MustParse("00000000-0000-0000-0000-0000000000a8")
)

// seasonEnd is the active season's last day, 31 May 2027, as its end is
// stored; termEnd is the end of that day in Istanbul.
var (
	seasonEnd = time.Date(2027, 5, 31, 0, 0, 0, 0, time.UTC)
	termEnd   = time.Date(2027, 5, 31, 23, 59, 59, 0, time.FixedZone("TRT", 3*3600))
)

type people struct {
	admin, yk1, yk2, dk1, lead, sublead, member, newcomer uuid.UUID
}

type fixture struct {
	pool     *pgxpool.Pool
	store    *membershipterm.PostgresStore
	dir      *identity.Memory
	seasons  *season.MemoryStore
	audit    *teamaudit.Postgres
	svc      *membershipterm.Service
	config   membershipterm.Config
	identity identity.Service
	mail     *mailSpy
	people   people
	mu       sync.Mutex
	now      time.Time
}

func (f *fixture) setNow(t time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = t.UTC()
}

func (f *fixture) clock() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

func newFixture(t *testing.T, mode membershipterm.Mode) *fixture {
	t.Helper()
	pool := testpostgres.Start(t)
	if err := migrate.Apply(context.Background(), pool); err != nil {
		t.Fatal(err)
	}
	f := &fixture{pool: pool, store: membershipterm.NewPostgresStore(pool), dir: identity.NewMemory(), seasons: season.NewMemoryStore(),
		audit: teamaudit.NewPostgres(pool), mail: &mailSpy{on: true}, now: time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC)}
	f.audit.SetClock(f.clock)
	for _, g := range []struct {
		id   uuid.UUID
		path string
	}{
		{gUyeler, "/UYELER"}, {gAdmin, "/ADMIN"}, {gYK, "/UYELER/YK"}, {gDK, "/UYELER/DK"}, {gArge, "/UYELER/ARGE"},
		{gWeblab, "/UYELER/ARGE/WEBLAB"}, {gWeblabL, "/UYELER/ARGE/WEBLAB/LIDERLER"},
		{gAI, "/UYELER/ARGE/WEBLAB/AI"}, {gAIL, "/UYELER/ARGE/WEBLAB/AI/LIDERLER"},
	} {
		f.dir.PutGroup(identity.Group{ID: g.id.String(), Name: g.path[strings.LastIndex(g.path, "/")+1:], Path: g.path})
	}
	users := user.NewMemoryStore()
	p := &f.people
	for _, person := range []struct {
		id    *uuid.UUID
		name  string
		group uuid.UUID
	}{
		{&p.admin, "Admin", gAdmin}, {&p.yk1, "Yekta", gYK}, {&p.yk2, "Yasemin", gYK}, {&p.dk1, "Deniz", gDK},
		{&p.lead, "Leyla", gWeblabL}, {&p.sublead, "Sena", gAIL}, {&p.member, "Mert", gWeblab}, {&p.newcomer, "Nur", uuid.Nil},
	} {
		*person.id = uuid.New()
		f.dir.PutUser(identity.Person{ID: *person.id, Email: person.name + "@example.test", FirstName: person.name, LastName: "Test"})
		if _, _, err := users.Upsert(context.Background(), user.User{ID: *person.id, Email: person.name + "@example.test", FirstName: person.name, LastName: "Test"}); err != nil {
			t.Fatal(err)
		}
		if person.group != uuid.Nil {
			if err := f.dir.AddMember(context.Background(), person.group.String(), *person.id); err != nil {
				t.Fatal(err)
			}
		}
	}
	start := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	if _, err := f.seasons.Create(context.Background(), season.Season{ID: uuid.New(), Name: "2026-2027", StartDate: &start, EndDate: &seasonEnd, Active: true}); err != nil {
		t.Fatal(err)
	}
	az := authz.NewAuthorizer(authz.DefaultPolicy())
	f.config = membershipterm.Config{
		Mode: mode, Weekly: true, Store: f.store, Directory: f.dir, Seasons: f.seasons,
		Rosters: teammail.DirectoryRosters{Directory: f.dir}, People: directoryPeople{f.dir}, Accounts: users,
		Mail: f.mail, Audit: f.audit, Authz: az, Now: f.clock, Logf: t.Logf,
	}
	svc, err := membershipterm.NewService(f.config)
	if err != nil {
		t.Fatal(err)
	}
	f.svc = svc
	f.identity = identity.NewServiceWithOptions(identity.AuditedDirectory(f.dir, f.audit, t.Logf), users, az, identity.Options{Membership: svc})
	svc.UseMemberships(f.identity)
	return f
}

// as is ctx for a request by id, a member of the Privileged Group path.
func as(id uuid.UUID, groups ...string) (context.Context, authz.Principal) {
	ctx := teamaudit.WithActor(context.Background(), teamaudit.Actor{ID: id, Basis: teamaudit.BasisPrivileged, CorrelationID: "test"})
	return ctx, authz.Principal{ID: id.String(), Groups: groups}
}

func (f *fixture) run(t *testing.T, mode membershipterm.Mode) membershipterm.Run {
	t.Helper()
	run, err := f.svc.Run(context.Background(), membershipterm.RunOptions{Mode: mode, Trigger: membershipterm.TriggerCLI})
	if err != nil {
		t.Fatal(err)
	}
	return run
}

// serviceReading is a second service on the same store that reads the
// Groups from dir: Keycloak answering otherwise than the memory directory.
func (f *fixture) serviceReading(t *testing.T, dir membershipterm.Directory) *membershipterm.Service {
	t.Helper()
	config := f.config
	config.Directory = dir
	svc, err := membershipterm.NewService(config)
	if err != nil {
		t.Fatal(err)
	}
	return svc
}

func (f *fixture) open(t *testing.T) []membershipterm.Term {
	t.Helper()
	terms, err := f.store.Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return terms
}

func (f *fixture) termOf(t *testing.T, person, group uuid.UUID) membershipterm.Term {
	t.Helper()
	for _, term := range f.open(t) {
		if term.UserID == person && term.GroupID == group {
			return term
		}
	}
	t.Fatalf("no open term of %s in %s", person, group)
	return membershipterm.Term{}
}

// queued are the mails waiting, by notice, as (recipient, subject) pairs.
func (f *fixture) queued(t *testing.T) map[membershipterm.Notice][][2]uuid.UUID {
	t.Helper()
	rows, err := f.pool.Query(context.Background(), `SELECT notice, recipient_id, COALESCE(subject_id, '00000000-0000-0000-0000-000000000000') FROM management_term_mails`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[membershipterm.Notice][][2]uuid.UUID{}
	for rows.Next() {
		var notice string
		var recipient, subject uuid.UUID
		if err := rows.Scan(&notice, &recipient, &subject); err != nil {
			t.Fatal(err)
		}
		out[membershipterm.Notice(notice)] = append(out[membershipterm.Notice(notice)], [2]uuid.UUID{recipient, subject})
	}
	return out
}

func (f *fixture) auditRows(t *testing.T, action teamaudit.Action) []map[string]any {
	t.Helper()
	rows, err := f.pool.Query(context.Background(), `
		SELECT actor_id::text, actor_basis, client, group_id::text, target_user_id::text, outcome, COALESCE(after->>'endsAt', ''), COALESCE(after->>'successorId', '')
		FROM team_audit_records WHERE action = $1 ORDER BY occurred_at, id`, string(action))
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var actor, basis, client, group, target, outcome, endsAt, successor string
		if err := rows.Scan(&actor, &basis, &client, &group, &target, &outcome, &endsAt, &successor); err != nil {
			t.Fatal(err)
		}
		out = append(out, map[string]any{"actor": actor, "basis": basis, "client": client, "group": group, "target": target,
			"outcome": outcome, "endsAt": endsAt, "successor": successor})
	}
	return out
}

func recipients(pairs [][2]uuid.UUID, subject uuid.UUID) []uuid.UUID {
	var out []uuid.UUID
	for _, p := range pairs {
		if p[1] == subject {
			out = append(out, p[0])
		}
	}
	slices.SortFunc(out, func(a, b uuid.UUID) int { return strings.Compare(a.String(), b.String()) })
	return out
}

func sorted(ids ...uuid.UUID) []uuid.UUID {
	slices.SortFunc(ids, func(a, b uuid.UUID) int { return strings.Compare(a.String(), b.String()) })
	return ids
}

// directoryPeople reads the mails' people from the memory directory.
type directoryPeople struct{ dir *identity.Memory }

func (p directoryPeople) Recipient(ctx context.Context, id uuid.UUID) (teammail.Recipient, teammail.SkipReason, error) {
	person, err := p.dir.GetUser(ctx, id)
	if err != nil {
		return teammail.Recipient{}, teammail.SkipInactive, nil
	}
	return teammail.Recipient{Email: person.Email, FullName: person.FirstName + " " + person.LastName}, "", nil
}

func (p directoryPeople) DisplayName(ctx context.Context, id uuid.UUID) string {
	person, err := p.dir.GetUser(ctx, id)
	if err != nil {
		return ""
	}
	return person.FirstName + " " + person.LastName
}

type sentMail struct {
	recipient string
	vars      map[string]string
}

type mailSpy struct {
	mu   sync.Mutex
	on   bool
	sent []sentMail
}

func (m *mailSpy) ManagementTermConfigured() bool { return m.on }

func (m *mailSpy) ManagementTerm(_ context.Context, recipient, _ string, vars map[string]string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sent = append(m.sent, sentMail{recipient: recipient, vars: vars})
	return nil
}
