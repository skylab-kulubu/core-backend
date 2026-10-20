package usersync

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/identity"
	"github.com/skylab-kulubu/core-backend/internal/mail"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// Directory is Keycloak's account list, read a page at a time.
type Directory interface {
	UsersPage(ctx context.Context, first, max int) ([]identity.Person, error)
}

// Accounts opens a person's row and gives it a Sky number (user.Service).
type Accounts interface {
	Ensure(ctx context.Context, id uuid.UUID, profile user.Profile) (user.User, bool, error)
}

// Mailer sends the Welcome mail and says what became of it (mail.SkyMail).
type Mailer interface {
	Welcome(ctx context.Context, u user.User) mail.WelcomeOutcome
}

// State is what the sync needs to know of a row.
type State struct {
	Email, FirstName, LastName, SkyNumber string
	// Active is account_state = 'active'; the sync leaves any other row as
	// it is.
	Active bool
	// Welcomed is welcome_sent_at set.
	Welcomed bool
}

// Rows is core's users table as the sync reads and marks it.
type Rows interface {
	// Lock takes the sync's database lock without waiting: false when
	// another core's pass holds it. release gives it back.
	Lock(ctx context.Context) (release func(), ok bool, err error)
	// States reads the rows of ids; a person without a row is missing.
	States(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]State, error)
	// FillMissing writes e-mail, first and last name where the active row
	// has none, never over a value, and reports whether it wrote.
	FillMissing(ctx context.Context, id uuid.UUID, email, firstName, lastName string) (bool, error)
	// ClaimWelcome marks the active row welcomed when it is not and has an
	// e-mail, and answers the row and the mark: ok false when another pass
	// did it first or the row cannot take the mail.
	ClaimWelcome(ctx context.Context, id uuid.UUID) (u user.User, at time.Time, ok bool, err error)
	// ReleaseWelcome takes back the mark at, after a mail that surely did not
	// go, so the next pass sends it.
	ReleaseWelcome(ctx context.Context, id uuid.UUID, at time.Time) error
}

const (
	// pageSize is how many accounts one Keycloak read asks for.
	pageSize = 100
	// pageDelay spaces the page reads, opDelay the Keycloak writes a new row
	// takes (Ensure reads and writes the skyNumber attribute): the realm is
	// not hurried for work that can wait.
	pageDelay = 200 * time.Millisecond
	opDelay   = 50 * time.Millisecond
	// maxEnsuresPerPass and maxWelcomesPerPass bound one pass; the rest is
	// the next pass's.
	maxEnsuresPerPass  = 100
	maxWelcomesPerPass = 25
	// wakeGap is the least time between two passes a wake asks for.
	wakeGap = 30 * time.Second
)

// Report is what one pass did. It names nobody.
type Report struct {
	// Locked: another core's pass held the lock, so this one did nothing.
	Locked   bool
	Seen     int
	Created  int
	Assigned int
	Filled   int
	Welcome  map[mail.WelcomeOutcome]int
	Deferred int
	Errors   int
}

func (r Report) changed() bool {
	if r.Created+r.Assigned+r.Filled+r.Deferred+r.Errors > 0 {
		return true
	}
	for _, n := range r.Welcome {
		if n > 0 {
			return true
		}
	}
	return false
}

// Worker runs the sync.
type Worker struct {
	dir      Directory
	accounts Accounts
	rows     Rows
	mailer   Mailer
	config   Config
	logf     func(string, ...any)
	wake     chan struct{}
	// pageDelay, opDelay and wakeGap are the package's; tests shorten them.
	pageDelay, opDelay, wakeGap time.Duration
	now                         func() time.Time

	mu          sync.Mutex
	passes      map[string]int64
	changes     map[string]int64
	welcome     map[mail.WelcomeOutcome]int64
	errors      int64
	deferred    int64
	lastSuccess time.Time
}

// New builds the worker. mailer may be nil: the rows and Sky numbers are
// kept and no Welcome mail is sent or marked.
func New(dir Directory, accounts Accounts, rows Rows, mailer Mailer, config Config, logf func(string, ...any)) *Worker {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &Worker{
		dir: dir, accounts: accounts, rows: rows, mailer: mailer, config: config, logf: logf,
		wake:      make(chan struct{}, 1),
		pageDelay: pageDelay, opDelay: opDelay, wakeGap: wakeGap, now: time.Now,
		passes:  map[string]int64{},
		changes: map[string]int64{},
		welcome: map[mail.WelcomeOutcome]int64{},
	}
}

// Wake asks for a pass soon (not sooner than wakeGap after the last). It
// never blocks, and a nil worker ignores it.
func (w *Worker) Wake() {
	if w == nil {
		return
	}
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

// Run passes once at once, then at every Interval and on a wake, until ctx
// ends; the channel closes when it has stopped.
func (w *Worker) Run(ctx context.Context) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(w.config.Interval)
		defer ticker.Stop()
		for {
			started := w.now()
			w.pass(ctx)
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			case <-w.wake:
				if wait := w.wakeGap - w.now().Sub(started); wait > 0 {
					select {
					case <-ctx.Done():
						return
					case <-time.After(wait):
					}
				}
			}
		}
	}()
	return done
}

// pass runs Pass and logs and counts it.
func (w *Worker) pass(ctx context.Context) {
	report, err := w.Pass(ctx)
	if ctx.Err() != nil {
		return
	}
	w.record(report, err)
	switch {
	case err != nil:
		w.logf(`{"event":"user_sync_failed","level":"warn","stage":%q,"seen":%d,"errors":%d}`, stageOf(err), report.Seen, report.Errors)
	case report.changed():
		w.logf(`{"event":"user_sync_pass","seen":%d,"created":%d,"sky_number_assigned":%d,"profile_filled":%d,"welcome_sent":%d,"welcome_refused":%d,"welcome_not_sent":%d,"welcome_unknown":%d,"deferred":%d,"errors":%d}`,
			report.Seen, report.Created, report.Assigned, report.Filled,
			report.Welcome[mail.WelcomeSent], report.Welcome[mail.WelcomeRefused], report.Welcome[mail.WelcomeNotSent], report.Welcome[mail.WelcomeUnknown],
			report.Deferred, report.Errors)
	}
}

// stageError says where a pass stopped; only the stage reaches the log.
type stageError struct {
	stage string
	err   error
}

func (e *stageError) Error() string { return e.stage + ": " + e.err.Error() }
func (e *stageError) Unwrap() error { return e.err }

func stageOf(err error) string {
	var s *stageError
	if errors.As(err, &s) {
		return s.stage
	}
	return "unknown"
}

// Pass runs one pass: it pages through Keycloak, and for every enabled
// person (service accounts left out) opens a missing row and gives a Sky
// number (Ensure), fills blanks from Keycloak, and sends the Welcome mail
// when the person is owed one. A failed page read stops the pass; a failure
// on one person is counted and the pass goes on. The next pass picks up
// whatever was left.
func (w *Worker) Pass(ctx context.Context) (Report, error) {
	report := Report{Welcome: map[mail.WelcomeOutcome]int{}}
	release, ok, err := w.rows.Lock(ctx)
	if err != nil {
		return report, &stageError{"lock", err}
	}
	if !ok {
		report.Locked = true
		return report, nil
	}
	defer release()
	welcomes := 0
	for first := 0; ; first += pageSize {
		page, err := w.dir.UsersPage(ctx, first, pageSize)
		if err != nil {
			return report, &stageError{"keycloak", err}
		}
		people := make([]identity.Person, 0, len(page))
		ids := make([]uuid.UUID, 0, len(page))
		for _, p := range page {
			if p.ServiceAccount || strings.HasPrefix(p.Username, "service-account-") {
				continue
			}
			people = append(people, p)
			ids = append(ids, p.ID)
		}
		states, err := w.rows.States(ctx, ids)
		if err != nil {
			return report, &stageError{"database", err}
		}
		for _, p := range people {
			if ctx.Err() != nil {
				return report, &stageError{"stopped", ctx.Err()}
			}
			report.Seen++
			if !p.Enabled {
				continue
			}
			st, has := states[p.ID]
			if has && !st.Active {
				continue
			}
			if !has || st.SkyNumber == "" {
				if report.Created+report.Assigned >= maxEnsuresPerPass {
					report.Deferred++
					continue
				}
				u, _, err := w.accounts.Ensure(ctx, p.ID, user.Profile{
					Email: p.Email, FirstName: p.FirstName, LastName: p.LastName, Username: p.Username,
				})
				w.sleep(ctx, w.opDelay)
				if errors.Is(err, user.ErrAccountBlocked) {
					continue
				}
				if err != nil || u.ID != p.ID {
					report.Errors++
					continue
				}
				if has {
					report.Assigned++
				} else {
					report.Created++
				}
				st = State{Email: u.Email, FirstName: u.FirstName, LastName: u.LastName, SkyNumber: u.SkyNumber, Active: true, Welcomed: st.Welcomed}
			}
			if (st.Email == "" && p.Email != "") || (st.FirstName == "" && p.FirstName != "") || (st.LastName == "" && p.LastName != "") {
				filled, err := w.rows.FillMissing(ctx, p.ID, p.Email, p.FirstName, p.LastName)
				if err != nil {
					report.Errors++
					continue
				}
				if filled {
					report.Filled++
					if st.Email == "" {
						st.Email = p.Email
					}
				}
			}
			if !w.owed(p, st) {
				continue
			}
			if welcomes >= maxWelcomesPerPass {
				report.Deferred++
				continue
			}
			welcomes++
			if err := w.sendWelcome(ctx, p.ID, &report); err != nil {
				report.Errors++
			}
		}
		if len(page) < pageSize {
			return report, nil
		}
		w.sleep(ctx, w.pageDelay)
	}
}

// owed: a mailer is configured, the row is not marked and has an e-mail,
// and Keycloak created the account on or after the cutoff. An account
// without a creation time is older than Keycloak's records of it and owed
// nothing.
func (w *Worker) owed(p identity.Person, st State) bool {
	return w.mailer != nil && !st.Welcomed && st.Email != "" &&
		p.CreatedAt != nil && !p.CreatedAt.Before(w.config.WelcomeSince)
}

// sendWelcome marks the row, then sends. The mark goes first so a crash
// between the two loses a mail rather than sending it twice; it is taken
// back only when the outcome says no mail task can exist.
func (w *Worker) sendWelcome(ctx context.Context, id uuid.UUID, report *Report) error {
	u, at, ok, err := w.rows.ClaimWelcome(ctx, id)
	if err != nil || !ok {
		return err
	}
	outcome := w.mailer.Welcome(ctx, u)
	report.Welcome[outcome]++
	if outcome == mail.WelcomeRefused || outcome == mail.WelcomeNotSent {
		// Not with ctx: a pass cut short still takes its mark back.
		releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		return w.rows.ReleaseWelcome(releaseCtx, id, at)
	}
	return nil
}

func (w *Worker) sleep(ctx context.Context, d time.Duration) {
	if d <= 0 {
		return
	}
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

func (w *Worker) record(report Report, err error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	switch {
	case err != nil:
		w.passes["failed"]++
	case report.Locked:
		w.passes["locked"]++
	default:
		w.passes["ok"]++
		w.lastSuccess = w.now()
	}
	w.changes["created"] += int64(report.Created)
	w.changes["sky_number_assigned"] += int64(report.Assigned)
	w.changes["profile_filled"] += int64(report.Filled)
	for outcome, n := range report.Welcome {
		w.welcome[outcome] += int64(n)
	}
	w.errors += int64(report.Errors)
	w.deferred += int64(report.Deferred)
}

// Prometheus is the sync's counters in Prometheus' text format. No person
// appears. A nil worker (the sync off) reports enabled 0.
func (w *Worker) Prometheus() string {
	var out strings.Builder
	if w == nil {
		out.WriteString("# TYPE skylab_user_sync_enabled gauge\nskylab_user_sync_enabled 0\n")
		return out.String()
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	out.WriteString("# TYPE skylab_user_sync_enabled gauge\nskylab_user_sync_enabled 1\n")
	welcomeOn := 0
	if w.mailer != nil {
		welcomeOn = 1
	}
	fmt.Fprintf(&out, "# TYPE skylab_user_sync_welcome_enabled gauge\nskylab_user_sync_welcome_enabled %d\n", welcomeOn)
	out.WriteString("# TYPE skylab_user_sync_passes_total counter\n")
	for _, result := range []string{"ok", "locked", "failed"} {
		fmt.Fprintf(&out, "skylab_user_sync_passes_total{result=%q} %d\n", result, w.passes[result])
	}
	out.WriteString("# TYPE skylab_user_sync_accounts_total counter\n")
	for _, change := range []string{"created", "sky_number_assigned", "profile_filled"} {
		fmt.Fprintf(&out, "skylab_user_sync_accounts_total{change=%q} %d\n", change, w.changes[change])
	}
	out.WriteString("# TYPE skylab_user_sync_welcome_total counter\n")
	for _, outcome := range mail.WelcomeOutcomes {
		fmt.Fprintf(&out, "skylab_user_sync_welcome_total{outcome=%q} %d\n", outcome, w.welcome[outcome])
	}
	fmt.Fprintf(&out, "# TYPE skylab_user_sync_errors_total counter\nskylab_user_sync_errors_total %d\n", w.errors)
	fmt.Fprintf(&out, "# TYPE skylab_user_sync_deferred_total counter\nskylab_user_sync_deferred_total %d\n", w.deferred)
	last := int64(0)
	if !w.lastSuccess.IsZero() {
		last = w.lastSuccess.Unix()
	}
	fmt.Fprintf(&out, "# TYPE skylab_user_sync_last_success_timestamp_seconds gauge\nskylab_user_sync_last_success_timestamp_seconds %d\n", last)
	return out.String()
}
