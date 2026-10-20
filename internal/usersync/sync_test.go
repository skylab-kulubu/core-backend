package usersync

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/identity"
	"github.com/skylab-kulubu/core-backend/internal/mail"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

var cutoff = time.Date(2026, 10, 18, 21, 0, 0, 0, time.UTC)

func at(t time.Time) *time.Time { return &t }

// fakeDir is Keycloak's account list.
type fakeDir struct {
	people []identity.Person
	err    error
	reads  int
}

func (d *fakeDir) UsersPage(_ context.Context, first, max int) ([]identity.Person, error) {
	d.reads++
	if d.err != nil {
		return nil, d.err
	}
	if first >= len(d.people) {
		return nil, nil
	}
	end := min(first+max, len(d.people))
	return d.people[first:end], nil
}

// fakeRows is the users table: it also serves Ensure, which opens a row with
// the next Sky number.
type fakeRows struct {
	mu       sync.Mutex
	rows     map[uuid.UUID]*row
	locked   bool
	next     int
	ensures  int
	ensureFn func(id uuid.UUID) error
}

type row struct {
	State
	username string
	at       time.Time
}

func newFakeRows() *fakeRows { return &fakeRows{rows: map[uuid.UUID]*row{}} }

func (r *fakeRows) Lock(context.Context) (func(), bool, error) {
	if r.locked {
		return nil, false, nil
	}
	r.locked = true
	return func() { r.locked = false }, true, nil
}

func (r *fakeRows) States(_ context.Context, ids []uuid.UUID) (map[uuid.UUID]State, error) {
	out := map[uuid.UUID]State{}
	for _, id := range ids {
		if got, ok := r.rows[id]; ok {
			out[id] = got.State
		}
	}
	return out, nil
}

func (r *fakeRows) FillMissing(_ context.Context, id uuid.UUID, email, first, last string) (bool, error) {
	got, ok := r.rows[id]
	if !ok || !got.Active {
		return false, nil
	}
	wrote := false
	fill := func(dst *string, v string) {
		if *dst == "" && v != "" {
			*dst = v
			wrote = true
		}
	}
	fill(&got.Email, email)
	fill(&got.FirstName, first)
	fill(&got.LastName, last)
	return wrote, nil
}

func (r *fakeRows) ClaimWelcome(_ context.Context, id uuid.UUID) (user.User, time.Time, bool, error) {
	got, ok := r.rows[id]
	if !ok || !got.Active || got.Welcomed || got.Email == "" {
		return user.User{}, time.Time{}, false, nil
	}
	got.Welcomed = true
	got.at = time.Now()
	return user.User{ID: id, Email: got.Email, FirstName: got.FirstName, LastName: got.LastName, SkyNumber: got.SkyNumber, Username: got.username}, got.at, true, nil
}

func (r *fakeRows) ReleaseWelcome(_ context.Context, id uuid.UUID, at time.Time) error {
	if got, ok := r.rows[id]; ok && got.Welcomed && got.at.Equal(at) {
		got.Welcomed = false
	}
	return nil
}

func (r *fakeRows) Ensure(_ context.Context, id uuid.UUID, p user.Profile) (user.User, bool, error) {
	r.ensures++
	if r.ensureFn != nil {
		if err := r.ensureFn(id); err != nil {
			return user.User{}, false, err
		}
	}
	got, ok := r.rows[id]
	created := !ok
	if !ok {
		got = &row{State: State{Email: p.Email, FirstName: p.FirstName, LastName: p.LastName, Active: true}, username: p.Username}
		r.rows[id] = got
	}
	if got.SkyNumber == "" {
		r.next++
		got.SkyNumber = fmt.Sprintf("SKY-%07d", r.next)
	}
	return user.User{ID: id, Email: got.Email, FirstName: got.FirstName, LastName: got.LastName, SkyNumber: got.SkyNumber}, created, nil
}

type fakeMailer struct {
	outcome mail.WelcomeOutcome
	sent    []user.User
}

func (m *fakeMailer) Welcome(_ context.Context, u user.User) mail.WelcomeOutcome {
	m.sent = append(m.sent, u)
	if m.outcome == "" {
		return mail.WelcomeSent
	}
	return m.outcome
}

func person(created *time.Time) identity.Person {
	id := uuid.New()
	return identity.Person{ID: id, Email: id.String()[:8] + "@example.com", FirstName: "Ada", LastName: "Lovelace",
		Username: "ada." + id.String()[:4], Enabled: true, CreatedAt: created}
}

func worker(dir *fakeDir, rows *fakeRows, mailer Mailer) *Worker {
	w := New(dir, rows, rows, mailer, Config{Enabled: true, Interval: time.Minute, WelcomeSince: cutoff}, nil)
	w.pageDelay, w.opDelay = 0, 0
	return w
}

// A person Keycloak has and core does not gets a row and a Sky number, and
// the Welcome mail when Keycloak created the account since the cutoff.
func TestPassOpensMissingRowsAndWelcomesNewAccounts(t *testing.T) {
	t.Parallel()
	recent := person(at(cutoff.Add(time.Hour)))
	old := person(at(cutoff.Add(-time.Hour)))
	undated := person(nil)
	dir := &fakeDir{people: []identity.Person{recent, old, undated}}
	rows := newFakeRows()
	mailer := &fakeMailer{}

	report, err := worker(dir, rows, mailer).Pass(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.Created != 3 || report.Seen != 3 || report.Welcome[mail.WelcomeSent] != 1 {
		t.Fatalf("report %+v", report)
	}
	for _, p := range []identity.Person{recent, old, undated} {
		if got := rows.rows[p.ID]; got == nil || got.SkyNumber == "" {
			t.Fatalf("%s: row %+v", p.Username, got)
		}
	}
	if len(mailer.sent) != 1 || mailer.sent[0].ID != recent.ID || mailer.sent[0].SkyNumber == "" || mailer.sent[0].Username != recent.Username {
		t.Fatalf("sent %+v", mailer.sent)
	}
	if !rows.rows[recent.ID].Welcomed || rows.rows[old.ID].Welcomed || rows.rows[undated.ID].Welcomed {
		t.Fatal("only the sent mail marks its row")
	}

	// A second pass changes nothing and sends nothing again.
	report, err = worker(dir, rows, mailer).Pass(context.Background())
	if err != nil || report.changed() || len(mailer.sent) != 1 {
		t.Fatalf("second pass %+v %v, sent %d", report, err, len(mailer.sent))
	}
}

// However the row was opened (a first request, the admin list), an owed
// mail goes once the row is unmarked; a marked row gets none.
func TestPassWelcomesUnmarkedRowsOnce(t *testing.T) {
	t.Parallel()
	opened := person(at(cutoff.Add(time.Minute)))
	welcomed := person(at(cutoff.Add(time.Minute)))
	rows := newFakeRows()
	rows.rows[opened.ID] = &row{State: State{Email: opened.Email, FirstName: "Ada", LastName: "Lovelace", SkyNumber: "SKY-0000100", Active: true}}
	rows.rows[welcomed.ID] = &row{State: State{Email: welcomed.Email, FirstName: "Ada", LastName: "Lovelace", SkyNumber: "SKY-0000101", Active: true, Welcomed: true}}
	mailer := &fakeMailer{}

	if _, err := worker(&fakeDir{people: []identity.Person{opened, welcomed}}, rows, mailer).Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(mailer.sent) != 1 || mailer.sent[0].ID != opened.ID || rows.ensures != 0 {
		t.Fatalf("sent %+v, ensures %d", mailer.sent, rows.ensures)
	}
}

// A row a first request opened without an e-mail (Account Center's token
// carries none) gets the address and names from Keycloak, never over a
// value, and then its Welcome mail.
func TestPassFillsBlanksThenWelcomes(t *testing.T) {
	t.Parallel()
	p := person(at(cutoff.Add(time.Hour)))
	rows := newFakeRows()
	rows.rows[p.ID] = &row{State: State{LastName: "Byron", SkyNumber: "SKY-0000007", Active: true}}
	mailer := &fakeMailer{}

	report, err := worker(&fakeDir{people: []identity.Person{p}}, rows, mailer).Pass(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got := rows.rows[p.ID]
	if report.Filled != 1 || got.Email != p.Email || got.FirstName != "Ada" || got.LastName != "Byron" {
		t.Fatalf("report %+v row %+v", report, got.State)
	}
	if len(mailer.sent) != 1 || mailer.sent[0].Email != p.Email {
		t.Fatalf("sent %+v", mailer.sent)
	}
}

// A row without a Sky number gets one; the rest of the row stays.
func TestPassAssignsMissingSkyNumber(t *testing.T) {
	t.Parallel()
	p := person(at(cutoff.Add(-time.Hour)))
	rows := newFakeRows()
	rows.rows[p.ID] = &row{State: State{Email: p.Email, FirstName: "Ada", LastName: "Lovelace", Active: true, Welcomed: true}}

	report, err := worker(&fakeDir{people: []identity.Person{p}}, rows, &fakeMailer{}).Pass(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.Assigned != 1 || report.Created != 0 || rows.rows[p.ID].SkyNumber == "" || !rows.rows[p.ID].Welcomed {
		t.Fatalf("report %+v row %+v", report, rows.rows[p.ID].State)
	}
}

// Disabled accounts, service accounts and rows that are not active are left
// alone.
func TestPassLeavesOthersAlone(t *testing.T) {
	t.Parallel()
	disabled := person(at(cutoff.Add(time.Hour)))
	disabled.Enabled = false
	service := person(at(cutoff.Add(time.Hour)))
	service.ServiceAccount = true
	serviceName := person(at(cutoff.Add(time.Hour)))
	serviceName.Username = "service-account-forms"
	pending := person(at(cutoff.Add(time.Hour)))
	rows := newFakeRows()
	rows.rows[pending.ID] = &row{State: State{Active: false}}
	mailer := &fakeMailer{}

	report, err := worker(&fakeDir{people: []identity.Person{disabled, service, serviceName, pending}}, rows, mailer).Pass(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rows.ensures != 0 || len(mailer.sent) != 0 || report.changed() || report.Seen != 2 {
		t.Fatalf("report %+v ensures %d sent %d", report, rows.ensures, len(mailer.sent))
	}
}

// A mail that surely did not go takes its mark back for the next pass; one
// that may have gone keeps it, so nobody gets it twice.
func TestPassReleasesTheMarkOnlyWhenNoMailCanExist(t *testing.T) {
	t.Parallel()
	for outcome, keep := range map[mail.WelcomeOutcome]bool{
		mail.WelcomeSent: true, mail.WelcomeUnknown: true, mail.WelcomeRefused: false, mail.WelcomeNotSent: false,
	} {
		p := person(at(cutoff.Add(time.Hour)))
		rows := newFakeRows()
		report, err := worker(&fakeDir{people: []identity.Person{p}}, rows, &fakeMailer{outcome: outcome}).Pass(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if rows.rows[p.ID].Welcomed != keep || report.Welcome[outcome] != 1 {
			t.Fatalf("%s: welcomed %v, report %+v", outcome, rows.rows[p.ID].Welcomed, report)
		}
	}
}

// Without a mailer the rows and Sky numbers are kept and nothing is marked.
func TestPassWithoutMailer(t *testing.T) {
	t.Parallel()
	p := person(at(cutoff.Add(time.Hour)))
	rows := newFakeRows()
	if _, err := worker(&fakeDir{people: []identity.Person{p}}, rows, nil).Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if rows.rows[p.ID].SkyNumber == "" || rows.rows[p.ID].Welcomed {
		t.Fatalf("row %+v", rows.rows[p.ID].State)
	}
}

// Another core's pass holds the lock: this one does nothing. A page that
// cannot be read stops the pass with the stage only.
func TestPassLockAndKeycloakFailure(t *testing.T) {
	t.Parallel()
	rows := newFakeRows()
	rows.locked = true
	dir := &fakeDir{people: []identity.Person{person(nil)}}
	report, err := worker(dir, rows, nil).Pass(context.Background())
	if err != nil || !report.Locked || dir.reads != 0 {
		t.Fatalf("locked: %+v %v reads %d", report, err, dir.reads)
	}

	rows.locked = false
	dir.err = errors.New("keycloak 503")
	_, err = worker(dir, rows, nil).Pass(context.Background())
	if stageOf(err) != "keycloak" {
		t.Fatalf("err %v", err)
	}
	if rows.locked {
		t.Fatal("a failed pass must give the lock back")
	}
}

// One person failing is counted and the pass goes on; a blocked account is
// no failure.
func TestPassGoesOnPastOnePerson(t *testing.T) {
	t.Parallel()
	failing, blocked, fine := person(nil), person(nil), person(nil)
	rows := newFakeRows()
	rows.ensureFn = func(id uuid.UUID) error {
		switch id {
		case failing.ID:
			return errors.New("database down")
		case blocked.ID:
			return user.ErrAccountBlocked
		}
		return nil
	}
	report, err := worker(&fakeDir{people: []identity.Person{failing, blocked, fine}}, rows, nil).Pass(context.Background())
	if err != nil || report.Errors != 1 || report.Created != 1 || rows.rows[fine.ID] == nil {
		t.Fatalf("report %+v %v", report, err)
	}
}

// One pass opens at most maxEnsuresPerPass rows and sends at most
// maxWelcomesPerPass mails, over several pages; the next pass does the rest.
func TestPassIsBounded(t *testing.T) {
	t.Parallel()
	var people []identity.Person
	for range maxEnsuresPerPass + 30 {
		people = append(people, person(at(cutoff.Add(time.Hour))))
	}
	dir := &fakeDir{people: people}
	rows := newFakeRows()
	mailer := &fakeMailer{}
	report, err := worker(dir, rows, mailer).Pass(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.Created != maxEnsuresPerPass || len(mailer.sent) != maxWelcomesPerPass || dir.reads != 2 {
		t.Fatalf("report %+v sent %d reads %d", report, len(mailer.sent), dir.reads)
	}
	for range 10 {
		if _, err := worker(dir, rows, mailer).Pass(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if len(rows.rows) != len(people) || len(mailer.sent) != len(people) {
		t.Fatalf("rows %d sent %d, want %d", len(rows.rows), len(mailer.sent), len(people))
	}
}

// The log lines and metrics carry counts and stages, never a person.
func TestLogsAndMetricsNameNobody(t *testing.T) {
	t.Parallel()
	p := person(at(cutoff.Add(time.Hour)))
	var lines []string
	w := New(&fakeDir{people: []identity.Person{p}}, newFakeRows(), nil, &fakeMailer{}, Config{Enabled: true, Interval: time.Minute, WelcomeSince: cutoff},
		func(format string, args ...any) { lines = append(lines, fmt.Sprintf(format, args...)) })
	rows := newFakeRows()
	w.accounts, w.rows = rows, rows
	w.pageDelay, w.opDelay = 0, 0
	w.pass(context.Background())
	w.dir = &fakeDir{err: fmt.Errorf("read %s: refused", p.Email)}
	w.pass(context.Background())
	text := strings.Join(lines, "\n") + w.Prometheus()
	for _, personal := range []string{p.Email, p.ID.String(), p.Username, "Ada", "SKY-"} {
		if strings.Contains(text, personal) {
			t.Fatalf("%q names %q", text, personal)
		}
	}
	for _, want := range []string{`"event":"user_sync_pass"`, `"welcome_sent":1`, `"stage":"keycloak"`,
		`skylab_user_sync_passes_total{result="ok"} 1`, `skylab_user_sync_passes_total{result="failed"} 1`,
		`skylab_user_sync_accounts_total{change="created"} 1`, `skylab_user_sync_welcome_total{outcome="sent"} 1`} {
		if !strings.Contains(text, want) {
			t.Fatalf("%q lacks %s", text, want)
		}
	}
	var none *Worker
	none.Wake()
	if !strings.Contains(none.Prometheus(), "skylab_user_sync_enabled 0") {
		t.Fatal("a nil worker reports the sync off")
	}
}

// Run passes at once and again on a wake, no sooner than the gap.
func TestRunPassesOnStartAndWake(t *testing.T) {
	t.Parallel()
	dir := &fakeDir{}
	rows := newFakeRows()
	w := worker(dir, rows, nil)
	w.config.Interval = time.Hour
	w.wakeGap = 0
	passes := make(chan struct{}, 4)
	w.logf = func(string, ...any) {}
	counting := &countingDir{fakeDir: dir, passes: passes}
	w.dir = counting
	ctx, cancel := context.WithCancel(context.Background())
	done := w.Run(ctx)
	waitPass := func() {
		select {
		case <-passes:
		case <-time.After(5 * time.Second):
			t.Fatal("no pass")
		}
	}
	waitPass()
	w.Wake()
	waitPass()
	cancel()
	<-done
}

type countingDir struct {
	*fakeDir
	passes chan struct{}
}

func (d *countingDir) UsersPage(ctx context.Context, first, max int) ([]identity.Person, error) {
	d.passes <- struct{}{}
	return d.fakeDir.UsersPage(ctx, first, max)
}
