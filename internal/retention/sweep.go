package retention

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Trigger is what started a run.
type Trigger string

const (
	TriggerSchedule Trigger = "schedule"
	TriggerCLI      Trigger = "cli"
)

// RunStatus is how a run ended.
type RunStatus string

const (
	RunRunning RunStatus = "running"
	// RunOK: every rule did its work (a dry run: counted).
	RunOK RunStatus = "ok"
	// RunPartial: the run ended, but a rule failed or was refused.
	RunPartial RunStatus = "partial"
	// RunFailed: the run stopped before it ended.
	RunFailed RunStatus = "failed"
	// RunAbandoned: a run whose process went away mid-run, found by the next.
	RunAbandoned RunStatus = "abandoned"
	// RunSkippedLocked: another run held the lock; this one did nothing.
	RunSkippedLocked RunStatus = "skipped_locked"
)

// RuleStatus is how one rule ended in a run.
type RuleStatus string

const (
	RuleOK RuleStatus = "ok"
	// RuleDryRun: counted, nothing changed.
	RuleDryRun RuleStatus = "dry_run"
	// RuleWouldRefuseLarge: a dry run whose apply the brake would refuse.
	RuleWouldRefuseLarge RuleStatus = "would_refuse_large"
	// RuleRefusedLarge: the brake refused the change; nothing changed.
	RuleRefusedLarge RuleStatus = "refused_large"
	RuleFailed       RuleStatus = "failed"
	// RuleNotApplicable: the rule has nothing to do in this configuration.
	RuleNotApplicable RuleStatus = "not_applicable"
)

// The brake: one run of a rule changes at most BrakeMaxRows rows, and at most
// BrakeMaxShare of its table once it would change more than BrakeMinRows. A
// wrong anchor (an Event end gone missing, a clock far off) can then never
// empty a table at once. The backlog of the first apply is cleared on
// purpose, from the command line with --allow-large.
const (
	BrakeMaxRows  = 50000
	BrakeMaxShare = 0.20
	// BrakeMinRows keeps the share from refusing the daily handful of a
	// small table (an Event's door staff), which would alarm every day and
	// teach everyone to pass --allow-large.
	BrakeMinRows = 100
)

// brakeRefuses reports whether the brake refuses changing matched rows of a
// table of tableRows rows.
func brakeRefuses(matched, tableRows int64) bool {
	if matched > BrakeMaxRows {
		return true
	}
	return matched > BrakeMinRows && float64(matched) > BrakeMaxShare*float64(tableRows)
}

const (
	// batchSize is how many rows one statement of apply changes.
	batchSize = 500
	// batchTimeout bounds one batch's statement.
	batchTimeout = 30 * time.Second
	// countTimeout bounds one count.
	countTimeout = 2 * time.Minute
	// scheduleEvery is the schedule's spacing: a run is due 23 hours after
	// the last successful one started, so an hourly check keeps runs at most
	// a day apart (ADR-0062: a rule's period plus a day).
	scheduleEvery = 23 * time.Hour
	// maxPeriodsClosedAtOnce bounds the catching up after a long outage.
	maxPeriodsClosedAtOnce = 64
)

// lockSQL takes the sweep's session advisory lock without waiting.
const lockSQL = `SELECT pg_try_advisory_lock(hashtextextended('core-backend retention sweep', 0))`

// ErrLocked is returned when another run holds the lock.
var ErrLocked = errors.New("retention sweep: another run holds the lock")

// ErrNotDue is returned by a run that asked to run only when due, and found,
// holding the lock, that another replica's run just made it not due.
var ErrNotDue = errors.New("retention sweep: not due")

// RunOptions say how to run.
type RunOptions struct {
	// Mode is dry-run or apply.
	Mode    Mode
	Trigger Trigger
	// Rule runs only the named rule. Such a run is not a full run: it does
	// not count as the day's run.
	Rule string
	// AllowLarge lets apply past the brake (the command line only).
	AllowLarge bool
	// OnlyIfDue makes the run check, once it holds the lock, that it is
	// still due (the schedule): two replicas that both found it due run it
	// once.
	OnlyIfDue bool
}

// RuleResult is one rule's part of a run.
type RuleResult struct {
	Rule           Rule
	Cutoff         *time.Time
	Matched        int64
	Changed        int64
	RelatedChanged int64
	Overdue        int64
	Anchorless     int64
	TableRows      *int64
	Status         RuleStatus
	ErrorCode      string
}

// ClosedPeriod is a period a run closed: the destruction record's unit.
type ClosedPeriod struct {
	ID        uuid.UUID
	StartedAt time.Time
	EndsAt    time.Time
	// Mode is what the period needed (periodModeSQL): an apply period needs
	// a successful apply run, a dry-run period a successful run of either.
	Mode        Mode
	ApplyRuns   int
	DryRuns     int
	RowsChanged int64
}

// Report is what a run did.
type Report struct {
	RunID         uuid.UUID
	Mode          Mode
	Status        RunStatus
	Rules         []RuleResult
	ClosedPeriods []ClosedPeriod
}

// Sweeper runs the sweep against core's database.
type Sweeper struct {
	pool   *pgxpool.Pool
	config Config
	now    func() time.Time
	logf   func(string, ...any)
}

// NewSweeper builds a sweeper. logf receives one line per rule and run, with
// counts only; nil discards them.
func NewSweeper(pool *pgxpool.Pool, config Config, logf func(string, ...any)) *Sweeper {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &Sweeper{pool: pool, config: config, now: time.Now, logf: logf}
}

// SetClock replaces the clock (tests).
func (s *Sweeper) SetClock(now func() time.Time) { s.now = now }

// Config is the sweeper's configuration.
func (s *Sweeper) Config() Config { return s.config }

// RuleNames are the names a run may be limited to, in run order.
func RuleNames(c Config) []string {
	rules := Rules(c, everySchema)
	names := make([]string, len(rules))
	for i, rule := range rules {
		names[i] = rule.Name
	}
	return names
}

// Due reports whether the scheduled run of mode is due: no full run of it
// has ended ok or partial in the last 23 hours. It reads the database, so a
// restart does not make a run due.
func (s *Sweeper) Due(ctx context.Context, mode Mode) (bool, error) {
	return due(ctx, s.pool, mode, s.now())
}

func due(ctx context.Context, db interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, mode Mode, now time.Time) (bool, error) {
	var last *time.Time
	if err := db.QueryRow(ctx, `
		SELECT max(started_at) FROM retention_runs
		WHERE mode = $1 AND full_run AND status IN ('ok', 'partial')`, string(mode)).Scan(&last); err != nil {
		return false, err
	}
	return last == nil || !now.Before(last.Add(scheduleEvery)), nil
}

// Run runs the sweep once. It returns ErrLocked, and a report with
// RunSkippedLocked, when another run holds the lock. A rule that fails or is
// refused does not stop the others; the run then ends partial.
func (s *Sweeper) Run(ctx context.Context, opts RunOptions) (Report, error) {
	if opts.Mode != ModeDryRun && opts.Mode != ModeApply {
		return Report{}, fmt.Errorf("retention sweep: mode %q cannot run", opts.Mode)
	}
	if opts.Trigger != TriggerSchedule && opts.Trigger != TriggerCLI {
		return Report{}, fmt.Errorf("retention sweep: unknown trigger %q", opts.Trigger)
	}
	if opts.AllowLarge && (opts.Mode != ModeApply || opts.Trigger != TriggerCLI) {
		return Report{}, errors.New("retention sweep: --allow-large is for an apply run from the command line")
	}
	if opts.Rule != "" && !knownRule(s.config, opts.Rule) {
		return Report{}, fmt.Errorf("retention sweep: no rule %q", opts.Rule)
	}
	report := Report{Mode: opts.Mode}

	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return report, err
	}
	// Closing the connection ends the session, which releases the lock even
	// when unlocking fails; the pool replaces it.
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = conn.Hijack().Close(closeCtx)
	}()

	var locked bool
	if err := conn.QueryRow(ctx, lockSQL).Scan(&locked); err != nil {
		return report, err
	}
	startedAt := s.now().UTC()
	if !locked {
		report.RunID = uuid.New()
		report.Status = RunSkippedLocked
		if _, err := conn.Exec(ctx, `
			INSERT INTO retention_runs (id, mode, triggered_by, full_run, allow_large, rule_set_version, started_at, finished_at, status)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $7, 'skipped_locked')`,
			report.RunID, string(opts.Mode), string(opts.Trigger), opts.Rule == "", opts.AllowLarge, RuleSetVersion, startedAt); err != nil {
			return report, err
		}
		s.logf("retention_run id=%s mode=%s trigger=%s status=%s", report.RunID, opts.Mode, opts.Trigger, report.Status)
		return report, ErrLocked
	}

	if opts.OnlyIfDue {
		if ok, err := due(ctx, conn, opts.Mode, startedAt); err != nil || !ok {
			if err == nil {
				err = ErrNotDue
			}
			return report, err
		}
	}
	// Holding the lock, any run still marked running is one whose process
	// went away.
	if _, err := conn.Exec(ctx, `
		UPDATE retention_runs SET status = 'abandoned', finished_at = $1, error_code = 'abandoned'
		WHERE status = 'running'`, startedAt); err != nil {
		return report, err
	}
	periodID, closed, err := s.advancePeriods(ctx, conn, startedAt)
	if err != nil {
		return report, err
	}
	report.ClosedPeriods = closed
	for _, period := range closed {
		s.logf("retention_period_closed period=%s started=%s ended=%s mode=%s apply_runs=%d dry_runs=%d rows_changed=%d",
			period.ID, period.StartedAt.Format(time.RFC3339), period.EndsAt.Format(time.RFC3339), period.Mode, period.ApplyRuns, period.DryRuns, period.RowsChanged)
	}

	report.RunID = uuid.New()
	if _, err := conn.Exec(ctx, `
		INSERT INTO retention_runs (id, period_id, mode, triggered_by, full_run, allow_large, rule_set_version, started_at, status)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 'running')`,
		report.RunID, periodID, string(opts.Mode), string(opts.Trigger), opts.Rule == "", opts.AllowLarge, RuleSetVersion, startedAt); err != nil {
		return report, err
	}
	finish := func(status RunStatus, code string) error {
		report.Status = status
		finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		_, err := conn.Exec(finishCtx, `
			UPDATE retention_runs SET status = $2, finished_at = $3, error_code = $4 WHERE id = $1`,
			report.RunID, string(status), s.now().UTC(), code)
		var changed int64
		for _, result := range report.Rules {
			changed += result.Changed + result.RelatedChanged
		}
		s.logf("retention_run id=%s mode=%s trigger=%s status=%s rules=%d changed=%d", report.RunID, opts.Mode, opts.Trigger, status, len(report.Rules), changed)
		return err
	}

	schema, err := readSchema(ctx, conn)
	if err != nil {
		return report, errors.Join(err, finish(RunFailed, errorCode(err)))
	}
	status := RunOK
	for _, rule := range Rules(s.config, schema) {
		if opts.Rule != "" && rule.Name != opts.Rule {
			continue
		}
		result := s.runRule(ctx, conn, rule, opts, startedAt)
		if err := s.recordRule(ctx, conn, report.RunID, result, startedAt); err != nil {
			report.Rules = append(report.Rules, result)
			return report, errors.Join(err, finish(RunFailed, errorCode(err)))
		}
		report.Rules = append(report.Rules, result)
		if result.Status == RuleFailed || result.Status == RuleRefusedLarge {
			status = RunPartial
		}
	}
	return report, finish(status, "")
}

func knownRule(c Config, name string) bool {
	for _, known := range RuleNames(c) {
		if known == name {
			return true
		}
	}
	return false
}

// readSchema reads what the rules depend on.
func readSchema(ctx context.Context, conn *pgxpool.Conn) (Schema, error) {
	var schema Schema
	err := conn.QueryRow(ctx, `SELECT to_regclass('public.contact_consents') IS NOT NULL`).Scan(&schema.ContactConsents)
	return schema, err
}

// runRule runs one rule at the run's time now. Its errors end up in the
// result as a code, never as text.
func (s *Sweeper) runRule(ctx context.Context, conn *pgxpool.Conn, rule Rule, opts RunOptions, now time.Time) RuleResult {
	result := RuleResult{Rule: rule}
	if rule.NotApplicable != "" {
		result.Status = RuleNotApplicable
		s.logRule(opts.Mode, result)
		return result
	}
	cutoff := now.Add(-rule.Period)
	result.Cutoff = &cutoff
	fail := func(err error) RuleResult {
		result.Status = RuleFailed
		result.ErrorCode = errorCode(err)
		s.logRule(opts.Mode, result)
		return result
	}
	var err error
	if result.Matched, err = count(ctx, conn, rule.countSQL(), cutoff); err != nil {
		return fail(err)
	}
	if query := rule.anchorlessSQL(); query != "" {
		if result.Anchorless, err = count(ctx, conn, query, cutoff); err != nil {
			return fail(err)
		}
	}
	if rule.Kind == KindAudit {
		// An audit's period already holds its grace: what it matches is
		// overdue.
		result.Overdue = result.Matched
		result.Status = RuleOK
		s.logRule(opts.Mode, result)
		return result
	}
	tableRows, err := count(ctx, conn, rule.tableRowsSQL())
	if err != nil {
		return fail(err)
	}
	result.TableRows = &tableRows
	refuse := brakeRefuses(result.Matched, tableRows)
	switch {
	case opts.Mode == ModeDryRun && refuse:
		result.Status = RuleWouldRefuseLarge
	case opts.Mode == ModeDryRun:
		result.Status = RuleDryRun
	case refuse && !opts.AllowLarge:
		result.Status = RuleRefusedLarge
	default:
		if result.Changed, result.RelatedChanged, err = applyRule(ctx, conn, rule, cutoff, result.Matched); err != nil {
			return fail(err)
		}
		result.Status = RuleOK
	}
	if result.Overdue, err = count(ctx, conn, rule.countSQL(), cutoff.Add(-overdueGrace)); err != nil {
		return fail(err)
	}
	s.logRule(opts.Mode, result)
	return result
}

// applyRule changes up to limit of the rows rule is due for at cutoff, a
// batch per transaction, and returns how many rows it changed, and how many
// of the related table. It never changes more than the count the brake
// allowed: a rows that becomes due during the run waits for the next one.
func applyRule(ctx context.Context, conn *pgxpool.Conn, rule Rule, cutoff time.Time, limit int64) (changed, related int64, err error) {
	query := rule.applySQL()
	for remaining := limit; remaining > 0; {
		batch := min(int64(batchSize), remaining)
		var n, m int64
		if err := inTx(ctx, conn, batchTimeout, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, query, cutoff, batch).Scan(&n, &m)
		}); err != nil {
			return changed, related, err
		}
		changed += n
		related += m
		remaining -= n
		// Fewer than asked: nothing else is due, or the rest is held by
		// another transaction until the next run.
		if n < batch {
			break
		}
	}
	return changed, related, nil
}

// count runs a count, with the cutoff when there is one.
func count(ctx context.Context, conn *pgxpool.Conn, query string, args ...any) (int64, error) {
	if !strings.Contains(query, "$1") {
		args = nil
	}
	var n int64
	err := inTx(ctx, conn, countTimeout, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, query, args...).Scan(&n)
	})
	return n, err
}

// inTx runs fn in its own transaction under a statement timeout.
func inTx(ctx context.Context, conn *pgxpool.Conn, timeout time.Duration, fn func(pgx.Tx) error) error {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if _, err := tx.Exec(ctx, fmt.Sprintf(`SET LOCAL statement_timeout = %d`, timeout.Milliseconds())); err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// recordRule writes one rule's part of a run.
func (s *Sweeper) recordRule(ctx context.Context, conn *pgxpool.Conn, runID uuid.UUID, result RuleResult, startedAt time.Time) error {
	recordCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	_, err := conn.Exec(recordCtx, `
		INSERT INTO retention_run_rules (
			run_id, rule, rule_version, kind, action, target_table, cutoff,
			matched, changed, related_changed, overdue, anchorless, table_rows,
			status, error_code, started_at, finished_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17)`,
		runID, result.Rule.Name, result.Rule.Version, string(result.Rule.Kind), string(result.Rule.Action), result.Rule.Table, result.Cutoff,
		result.Matched, result.Changed, result.RelatedChanged, result.Overdue, result.Anchorless, result.TableRows,
		string(result.Status), result.ErrorCode, startedAt, s.now().UTC())
	return err
}

func (s *Sweeper) logRule(mode Mode, result RuleResult) {
	cutoff := "-"
	if result.Cutoff != nil {
		cutoff = result.Cutoff.UTC().Format(time.RFC3339)
	}
	code := result.ErrorCode
	if code == "" {
		code = "-"
	}
	s.logf("retention_rule rule=%s version=%d mode=%s status=%s cutoff=%s matched=%d changed=%d related=%d overdue=%d anchorless=%d code=%s",
		result.Rule.Name, result.Rule.Version, mode, result.Status, cutoff, result.Matched, result.Changed, result.RelatedChanged,
		result.Overdue, result.Anchorless, code)
}

// periodModeSQL is the mode period p needed, with core's configured mode as
// $3: apply when one of its scheduled runs was an apply run (the schedule
// runs in the configured mode, failed runs included), or, when the schedule
// did not run in it at all, when core is in apply mode as it closes; dry-run
// otherwise. So a rollout period of scheduled dry runs needs a dry run even
// if apply is switched on before it closes, and a period in apply mode is
// not met by dry runs.
const periodModeSQL = `CASE
	WHEN EXISTS (SELECT 1 FROM retention_runs r WHERE r.period_id = p.id AND r.triggered_by = 'schedule' AND r.mode = 'apply') THEN 'apply'
	WHEN NOT EXISTS (SELECT 1 FROM retention_runs r WHERE r.period_id = p.id AND r.triggered_by = 'schedule') AND $3 = 'apply' THEN 'apply'
	ELSE 'dry-run' END`

// advancePeriods returns the open period at now: it opens the first one, and
// closes every period that ended, each followed by the next where it ended.
func (s *Sweeper) advancePeriods(ctx context.Context, conn *pgxpool.Conn, now time.Time) (uuid.UUID, []ClosedPeriod, error) {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return uuid.Nil, nil, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	var (
		id               uuid.UUID
		startedAt, endAt time.Time
		closed           []ClosedPeriod
	)
	err = tx.QueryRow(ctx, `SELECT id, started_at, ends_at FROM retention_periods WHERE closed_at IS NULL FOR UPDATE`).
		Scan(&id, &startedAt, &endAt)
	if errors.Is(err, pgx.ErrNoRows) {
		id, startedAt, endAt = uuid.New(), now, now.Add(s.config.Period)
		if _, err := tx.Exec(ctx, `INSERT INTO retention_periods (id, started_at, ends_at) VALUES ($1, $2, $3)`, id, startedAt, endAt); err != nil {
			return uuid.Nil, nil, err
		}
	} else if err != nil {
		return uuid.Nil, nil, err
	}
	for !now.Before(endAt) {
		if len(closed) == maxPeriodsClosedAtOnce {
			return uuid.Nil, nil, errors.New("retention sweep: too many periods ended at once")
		}
		period := ClosedPeriod{ID: id, StartedAt: startedAt, EndsAt: endAt}
		var mode string
		if err := tx.QueryRow(ctx, `
			UPDATE retention_periods p SET closed_at = $2, mode = `+periodModeSQL+`,
				apply_runs = (SELECT count(*) FROM retention_runs r
					WHERE r.period_id = p.id AND r.full_run AND r.mode = 'apply' AND r.status IN ('ok', 'partial')),
				dry_runs = (SELECT count(*) FROM retention_runs r
					WHERE r.period_id = p.id AND r.full_run AND r.mode = 'dry-run' AND r.status IN ('ok', 'partial')),
				rows_changed = (SELECT COALESCE(sum(rr.changed + rr.related_changed), 0) FROM retention_run_rules rr
					JOIN retention_runs r ON r.id = rr.run_id WHERE r.period_id = p.id)
			WHERE p.id = $1
			RETURNING mode, apply_runs, dry_runs, rows_changed`, id, now, string(s.config.Mode)).Scan(&mode, &period.ApplyRuns, &period.DryRuns, &period.RowsChanged); err != nil {
			return uuid.Nil, nil, err
		}
		period.Mode = Mode(mode)
		closed = append(closed, period)
		id, startedAt, endAt = uuid.New(), endAt, endAt.Add(s.config.Period)
		if _, err := tx.Exec(ctx, `INSERT INTO retention_periods (id, started_at, ends_at) VALUES ($1, $2, $3)`, id, startedAt, endAt); err != nil {
			return uuid.Nil, nil, err
		}
	}
	return id, closed, tx.Commit(ctx)
}

// errorCode is an error as a code for the record and the log: a
// PostgreSQL error's SQLSTATE, never its message, which can quote a value.
func errorCode(err error) string {
	var pgErr *pgconn.PgError
	switch {
	case errors.As(err, &pgErr):
		return "sqlstate_" + strings.ToLower(pgErr.Code)
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "canceled"
	default:
		return "error"
	}
}
