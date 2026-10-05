package retention

import (
	"strings"
	"time"
)

// RuleSetVersion changes whenever a rule is added, removed or changes its
// version. A run records it.
const RuleSetVersion = 2

// Kind is what a rule does.
type Kind string

const (
	// KindSweep rules change rows in apply mode and count them in a dry run.
	KindSweep Kind = "sweep"
	// KindAudit rules only count: the rows another of core's cleanups should
	// already have removed, so the record covers that cleanup too.
	KindAudit Kind = "audit"
)

// Action is what a sweep rule does to a row.
type Action string

const (
	// ActionScrub empties the row's personal fields and keeps the row: a
	// durable record stays and the person goes (ADR-0042).
	ActionScrub Action = "scrub"
	// ActionDelete deletes the row: a relationship row whose removal is the
	// fact itself (ADR-0042).
	ActionDelete Action = "delete"
	// ActionCount is an audit's: nothing changes.
	ActionCount Action = "count"
)

// overdueGrace is how much later than the cutoff a row counts as overdue:
// the policy's "at the latest" is a rule's period plus a day (ADR-0062).
const overdueGrace = 24 * time.Hour

// A Rule is one retention rule. A sweep rule's where clause is the single
// definition of the rows it is due for at a cutoff: the dry run counts it,
// apply changes the rows it selects, a batch at a time, and overdue is the
// same count a day earlier. The clause leaves out every row the rule has
// already handled, so a second run finds nothing.
type Rule struct {
	// Name and Version identify the rule in the record. A rule that changes
	// what it selects or does gets a new version.
	Name    string
	Version int
	Kind    Kind
	Action  Action
	// Table is the table the rule changes or counts, and the brake's measure.
	Table string
	// RelatedTable is the table the related statement changes ("" for none).
	RelatedTable string
	// Period is the age past which a row is due: the cutoff is now - Period.
	// An audit's period includes a day's grace for its hourly cleanup.
	Period time.Duration
	// Alarm is set on an audit whose remaining rows mean its cleanup is not
	// working. Sweep rules alarm on rows still overdue after an apply run.
	Alarm bool
	// NotApplicable, when set, says why the rule has nothing to do in this
	// configuration. Such a rule runs no query.
	NotApplicable string

	alias string
	// key identifies a row of Table for a batch: a primary key column, or
	// ctid for a table without one.
	key string
	// where is the rule's due clause over Table AS alias, with the cutoff
	// as $1.
	where string
	// set is a scrub's assignment list.
	set string
	// related is a data-modifying statement run in the same statement as
	// each batch, over the batch's keys (the CTE batch(k)). It returns one
	// row per row it changed.
	related string
	// anchorless counts, with the cutoff as $1 or not at all, the rows the
	// rule cannot date. They are never changed, only counted.
	anchorless string
}

// countSQL counts the rows the rule is due for at the cutoff $1.
func (r Rule) countSQL() string {
	return `SELECT count(*) FROM ` + r.Table + ` ` + r.alias + ` WHERE ` + r.where
}

// anchorlessSQL counts the rows the rule cannot date; "" when it dates every
// row.
func (r Rule) anchorlessSQL() string {
	if r.anchorless == "" {
		return ""
	}
	return `SELECT count(*) FROM ` + r.Table + ` ` + r.alias + ` WHERE ` + r.anchorless
}

// applySQL changes at most $2 of the rows the rule is due for at the cutoff
// $1, skipping rows another transaction holds, and returns how many rows it
// changed and how many of the related table. The rows are chosen by the same
// where clause countSQL counts.
func (r Rule) applySQL() string {
	var b strings.Builder
	b.WriteString(`WITH batch AS (SELECT ` + r.alias + `.` + r.key + ` AS k FROM ` + r.Table + ` ` + r.alias +
		` WHERE ` + r.where + ` LIMIT $2 FOR UPDATE OF ` + r.alias + ` SKIP LOCKED)`)
	related := `0::bigint`
	if r.related != "" {
		b.WriteString(`, related AS (` + r.related + ` RETURNING 1)`)
		related = `(SELECT count(*) FROM related)`
	}
	switch r.Action {
	case ActionScrub:
		b.WriteString(`, changed AS (UPDATE ` + r.Table + ` ` + r.alias + ` SET ` + r.set +
			` FROM batch WHERE ` + r.alias + `.` + r.key + ` = batch.k RETURNING 1)`)
	case ActionDelete:
		b.WriteString(`, changed AS (DELETE FROM ` + r.Table + ` ` + r.alias +
			` USING batch WHERE ` + r.alias + `.` + r.key + ` = batch.k RETURNING 1)`)
	default:
		return ""
	}
	b.WriteString(` SELECT (SELECT count(*) FROM changed), ` + related)
	return b.String()
}

// tableRowsSQL counts the rule's table, the brake's measure.
func (r Rule) tableRowsSQL() string {
	return `SELECT count(*) FROM ` + r.Table
}
