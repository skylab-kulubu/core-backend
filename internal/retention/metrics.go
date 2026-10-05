package retention

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// staleAfter is when the lack of a successful run alarms: two daily runs
// missed.
const staleAfter = 48 * time.Hour

// Attention reasons: each is a state that needs a person.
const (
	// AttentionStale: no successful full run in the configured mode for two
	// days.
	AttentionStale = "stale"
	// AttentionRunFailed: the latest run stopped before it ended.
	AttentionRunFailed = "run_failed"
	// AttentionRuleFailed: a rule failed in the latest run.
	AttentionRuleFailed = "rule_failed"
	// AttentionRefusedLarge: the brake refused a rule in the latest run.
	AttentionRefusedLarge = "refused_large"
	// AttentionOverdue: rows a day past their period remain: after an
	// apply run's rule, or for an audit whose cleanup should leave none.
	AttentionOverdue = "overdue"
	// AttentionPeriodWithoutRun: the last period closed without a
	// successful full run.
	AttentionPeriodWithoutRun = "period_without_run"
)

// Attention is one state that needs a person. It names a rule, never a row.
type Attention struct {
	Reason string
	Rule   string
}

// LogAttention writes each attention as one fixed-format line.
func LogAttention(logger *log.Logger) func(Attention) {
	return func(a Attention) {
		rule := a.Rule
		if rule == "" {
			rule = "-"
		}
		logger.Printf("retention_attention reason=%s rule=%s", a.Reason, rule)
	}
}

// Metrics are the sweep's metrics on /v1/metrics. They are read from the
// records, not kept by the process, so every replica reports the same
// values and a restart loses none. Nothing is rendered before the first
// successful read, so a replica that cannot read the records never reports
// a reassuring zero.
type Metrics struct {
	pool      *pgxpool.Pool
	config    Config
	now       func() time.Time
	attention func(Attention)

	mu        sync.Mutex
	snapshot  *snapshot
	announced map[Attention]bool
}

type ruleRow struct {
	rule, kind, status string
	matched, overdue   int64
}

type changedKey struct{ rule, table string }

type snapshot struct {
	lastSuccess    map[Mode]time.Time
	latestMode     Mode
	latestStatus   RunStatus
	latestRules    []ruleRow
	changed        map[changedKey]int64
	failures       map[string]int64
	periodEndsAt   *time.Time
	lastClosedRuns *int64
	attention      []Attention
}

// NewMetrics builds the metrics. attention receives each attention once
// while it lasts in this process; nil logs it.
func NewMetrics(pool *pgxpool.Pool, config Config, attention func(Attention)) *Metrics {
	if attention == nil {
		attention = LogAttention(log.Default())
	}
	return &Metrics{pool: pool, config: config, now: time.Now, attention: attention, announced: map[Attention]bool{}}
}

// SetClock replaces the clock (tests).
func (m *Metrics) SetClock(now func() time.Time) { m.now = now }

// Refresh reads the records. A failed read keeps the last values.
func (m *Metrics) Refresh(ctx context.Context) error {
	s := &snapshot{lastSuccess: map[Mode]time.Time{}, changed: map[changedKey]int64{}, failures: map[string]int64{}}
	now := m.now().UTC()

	rows, err := m.pool.Query(ctx, `
		SELECT mode, max(started_at) FROM retention_runs
		WHERE full_run AND status IN ('ok', 'partial') GROUP BY mode`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var mode string
		var at time.Time
		if err := rows.Scan(&mode, &at); err != nil {
			rows.Close()
			return err
		}
		s.lastSuccess[Mode(mode)] = at
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	// The latest full run that ended (not one still running, skipped or
	// abandoned by a process that went away).
	var latestID *uuid.UUID
	var latestMode, latestStatus string
	err = m.pool.QueryRow(ctx, `
		SELECT id, mode, status FROM retention_runs
		WHERE full_run AND status IN ('ok', 'partial', 'failed')
		ORDER BY started_at DESC LIMIT 1`).Scan(&latestID, &latestMode, &latestStatus)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if latestID != nil {
		s.latestMode, s.latestStatus = Mode(latestMode), RunStatus(latestStatus)
		rows, err := m.pool.Query(ctx, `
			SELECT rule, kind, status, matched, overdue FROM retention_run_rules WHERE run_id = $1 ORDER BY rule`, *latestID)
		if err != nil {
			return err
		}
		for rows.Next() {
			var r ruleRow
			if err := rows.Scan(&r.rule, &r.kind, &r.status, &r.matched, &r.overdue); err != nil {
				rows.Close()
				return err
			}
			s.latestRules = append(s.latestRules, r)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
	}

	// Counters: the records are never deleted, so their sums only grow.
	rows, err = m.pool.Query(ctx, `
		SELECT rr.rule, rr.target_table, sum(rr.changed), sum(rr.related_changed)
		FROM retention_run_rules rr JOIN retention_runs r ON r.id = rr.run_id
		WHERE r.mode = 'apply' AND rr.kind = 'sweep'
		GROUP BY rr.rule, rr.target_table`)
	if err != nil {
		return err
	}
	related := relatedTables(m.config)
	for rows.Next() {
		var rule, table string
		var changed, relatedChanged int64
		if err := rows.Scan(&rule, &table, &changed, &relatedChanged); err != nil {
			rows.Close()
			return err
		}
		s.changed[changedKey{rule, table}] += changed
		if other := related[rule]; other != "" {
			s.changed[changedKey{rule, other}] += relatedChanged
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	rows, err = m.pool.Query(ctx, `SELECT rule, count(*) FROM retention_run_rules WHERE status = 'failed' GROUP BY rule`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var rule string
		var n int64
		if err := rows.Scan(&rule, &n); err != nil {
			rows.Close()
			return err
		}
		s.failures[rule] = n
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	if err := m.pool.QueryRow(ctx, `
		SELECT (SELECT ends_at FROM retention_periods WHERE closed_at IS NULL),
			(SELECT apply_runs + dry_runs FROM retention_periods WHERE closed_at IS NOT NULL ORDER BY ends_at DESC LIMIT 1)`).
		Scan(&s.periodEndsAt, &s.lastClosedRuns); err != nil {
		return err
	}

	// When the configured mode began: its first run after the latest run
	// in another mode. Before its first run nothing is stale yet.
	var modeSince *time.Time
	if err := m.pool.QueryRow(ctx, `
		SELECT min(started_at) FROM retention_runs
		WHERE mode = $1 AND started_at > COALESCE(
			(SELECT max(started_at) FROM retention_runs WHERE mode <> $1), '-infinity'::timestamptz)`,
		string(m.config.Mode)).Scan(&modeSince); err != nil {
		return err
	}
	s.attention = m.attentionOf(s, modeSince, now)

	m.mu.Lock()
	m.snapshot = s
	current := map[Attention]bool{}
	var fresh []Attention
	for _, a := range s.attention {
		current[a] = true
		if !m.announced[a] {
			fresh = append(fresh, a)
		}
	}
	m.announced = current
	m.mu.Unlock()
	for _, a := range fresh {
		m.attention(a)
	}
	return nil
}

func relatedTables(c Config) map[string]string {
	out := map[string]string{}
	for _, rule := range Rules(c, everySchema) {
		if rule.RelatedTable != "" {
			out[rule.Name] = rule.RelatedTable
		}
	}
	return out
}

// attentionOf lists the states of s that need a person, in a fixed order.
func (m *Metrics) attentionOf(s *snapshot, modeSince *time.Time, now time.Time) []Attention {
	var out []Attention
	if m.config.Mode == ModeOff {
		return nil
	}
	if last, ok := s.lastSuccess[m.config.Mode]; ok {
		if now.Sub(last) > staleAfter {
			out = append(out, Attention{Reason: AttentionStale})
		}
	} else if modeSince != nil && now.Sub(*modeSince) > staleAfter {
		out = append(out, Attention{Reason: AttentionStale})
	}
	if s.latestStatus == RunFailed {
		out = append(out, Attention{Reason: AttentionRunFailed})
	}
	alarms := map[string]bool{}
	for _, rule := range Rules(m.config, everySchema) {
		alarms[rule.Name] = rule.Kind == KindAudit && rule.Alarm
	}
	for _, r := range s.latestRules {
		switch RuleStatus(r.status) {
		case RuleFailed:
			out = append(out, Attention{Reason: AttentionRuleFailed, Rule: r.rule})
		case RuleRefusedLarge:
			out = append(out, Attention{Reason: AttentionRefusedLarge, Rule: r.rule})
		}
		if r.overdue > 0 && ((Kind(r.kind) == KindSweep && s.latestMode == ModeApply) || (Kind(r.kind) == KindAudit && alarms[r.rule])) {
			out = append(out, Attention{Reason: AttentionOverdue, Rule: r.rule})
		}
	}
	if s.lastClosedRuns != nil && *s.lastClosedRuns == 0 {
		out = append(out, Attention{Reason: AttentionPeriodWithoutRun})
	}
	return out
}

// Prometheus renders the metrics in the text exposition format. Labels are
// modes, rules, tables and statuses: never a value a row held.
func (m *Metrics) Prometheus() string {
	if m == nil {
		return ""
	}
	m.mu.Lock()
	s := m.snapshot
	m.mu.Unlock()
	if s == nil {
		return ""
	}
	now := m.now().UTC()
	var b strings.Builder
	gauge := func(name string) { b.WriteString("# TYPE " + name + " gauge\n") }
	counter := func(name string) { b.WriteString("# TYPE " + name + " counter\n") }
	line := func(name, labels string, value int64) {
		if labels != "" {
			labels = "{" + labels + "}"
		}
		b.WriteString(name + labels + " " + strconv.FormatInt(value, 10) + "\n")
	}

	gauge("skylab_retention_mode")
	for _, mode := range []Mode{ModeOff, ModeDryRun, ModeApply} {
		value := int64(0)
		if mode == m.config.Mode {
			value = 1
		}
		line("skylab_retention_mode", `mode="`+string(mode)+`"`, value)
	}
	gauge("skylab_retention_attention")
	line("skylab_retention_attention", "", int64(len(s.attention)))
	if len(s.lastSuccess) > 0 {
		gauge("skylab_retention_last_success_timestamp_seconds")
		for _, mode := range []Mode{ModeDryRun, ModeApply} {
			if at, ok := s.lastSuccess[mode]; ok {
				line("skylab_retention_last_success_timestamp_seconds", `mode="`+string(mode)+`"`, at.Unix())
			}
		}
	}
	if s.periodEndsAt != nil {
		gauge("skylab_retention_period_seconds_left")
		line("skylab_retention_period_seconds_left", "", max(0, int64(s.periodEndsAt.Sub(now)/time.Second)))
	}
	if len(s.latestRules) > 0 {
		gauge("skylab_retention_rows_matched")
		for _, r := range s.latestRules {
			line("skylab_retention_rows_matched", `rule="`+r.rule+`"`, r.matched)
		}
		gauge("skylab_retention_overdue_rows")
		for _, r := range s.latestRules {
			line("skylab_retention_overdue_rows", `rule="`+r.rule+`"`, r.overdue)
		}
		gauge("skylab_retention_refused_large")
		for _, r := range s.latestRules {
			refused := int64(0)
			if RuleStatus(r.status) == RuleRefusedLarge {
				refused = 1
			}
			line("skylab_retention_refused_large", `rule="`+r.rule+`"`, refused)
		}
		gauge("skylab_retention_rule_status")
		for _, r := range s.latestRules {
			line("skylab_retention_rule_status", `rule="`+r.rule+`",status="`+r.status+`"`, 1)
		}
	}
	if len(s.changed) > 0 {
		counter("skylab_retention_rows_changed_total")
		keys := make([]changedKey, 0, len(s.changed))
		for key := range s.changed {
			keys = append(keys, key)
		}
		sort.Slice(keys, func(i, j int) bool {
			if keys[i].rule != keys[j].rule {
				return keys[i].rule < keys[j].rule
			}
			return keys[i].table < keys[j].table
		})
		for _, key := range keys {
			line("skylab_retention_rows_changed_total", fmt.Sprintf(`rule="%s",table="%s"`, key.rule, key.table), s.changed[key])
		}
	}
	if len(s.failures) > 0 {
		counter("skylab_retention_rule_failures_total")
		rules := make([]string, 0, len(s.failures))
		for rule := range s.failures {
			rules = append(rules, rule)
		}
		sort.Strings(rules)
		for _, rule := range rules {
			line("skylab_retention_rule_failures_total", `rule="`+rule+`"`, s.failures[rule])
		}
	}
	return b.String()
}

// Attention lists the current attention (tests and the command line).
func (m *Metrics) Attention() []Attention {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.snapshot == nil {
		return nil
	}
	return append([]Attention(nil), m.snapshot.attention...)
}
