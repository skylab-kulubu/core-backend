-- Periodic destruction records (ADR-0062, docs/retention-sweep.md): what the
-- retention sweep did, when, under which rule and how many rows, and the
-- periods (PERIODIC_DESTRUCTION_INTERVAL) they add up to. The record of a
-- deletion, destruction or anonymisation is kept at least three years (KVKK
-- deletion regulation art. 7(3)); no code path deletes these rows
-- (TestNoCodePathDeletesRetentionRecords), and the down migration refuses
-- while any exists.
--
-- No personal data: no address, name, IP, subject, Ticket, Event or row id,
-- and no free text. Rules, tables and error codes are identifiers the CHECKs
-- below hold to a fixed shape, so nothing a row held can be written here.
-- Only new tables: nothing existing is locked or scanned.

-- A period: the unit of the destruction record and of the alarm. The first
-- run opens one; the first run after its end closes it, writes its totals,
-- and opens the next one where it ended.
CREATE TABLE IF NOT EXISTS retention_periods (
    id UUID PRIMARY KEY,
    started_at TIMESTAMPTZ NOT NULL,
    ends_at TIMESTAMPTZ NOT NULL,
    closed_at TIMESTAMPTZ,
    -- Written when the period closes: its successful full runs by mode and
    -- the rows its runs changed.
    apply_runs INTEGER,
    dry_runs INTEGER,
    rows_changed BIGINT,
    CONSTRAINT retention_periods_span_check CHECK (ends_at > started_at),
    CONSTRAINT retention_periods_closed_check CHECK (
        (closed_at IS NULL) = (apply_runs IS NULL)
        AND (closed_at IS NULL) = (dry_runs IS NULL)
        AND (closed_at IS NULL) = (rows_changed IS NULL)
        AND (apply_runs IS NULL OR apply_runs >= 0)
        AND (dry_runs IS NULL OR dry_runs >= 0)
        AND (rows_changed IS NULL OR rows_changed >= 0)
    )
);

-- At most one open period.
CREATE UNIQUE INDEX IF NOT EXISTS retention_periods_open_idx
    ON retention_periods ((closed_at IS NULL)) WHERE closed_at IS NULL;

-- A run: one pass of the sweep, scheduled or from the command line.
CREATE TABLE IF NOT EXISTS retention_runs (
    id UUID PRIMARY KEY,
    -- NULL for a run that found the sweep already running elsewhere.
    period_id UUID REFERENCES retention_periods (id),
    mode TEXT NOT NULL,
    triggered_by TEXT NOT NULL,
    -- false when the command ran one rule (--rule): such a run does not
    -- count as the day's run.
    full_run BOOLEAN NOT NULL,
    allow_large BOOLEAN NOT NULL DEFAULT false,
    rule_set_version INTEGER NOT NULL,
    started_at TIMESTAMPTZ NOT NULL,
    finished_at TIMESTAMPTZ,
    status TEXT NOT NULL,
    error_code TEXT NOT NULL DEFAULT '',
    CONSTRAINT retention_runs_mode_check CHECK (mode IN ('dry-run', 'apply')),
    CONSTRAINT retention_runs_trigger_check CHECK (triggered_by IN ('schedule', 'cli')),
    CONSTRAINT retention_runs_status_check CHECK (
        status IN ('running', 'ok', 'partial', 'failed', 'abandoned', 'skipped_locked')
    ),
    CONSTRAINT retention_runs_finished_check CHECK ((status = 'running') = (finished_at IS NULL)),
    CONSTRAINT retention_runs_error_code_check CHECK (error_code ~ '^[a-z0-9_]{0,64}$'),
    CONSTRAINT retention_runs_version_check CHECK (rule_set_version > 0)
);

CREATE INDEX IF NOT EXISTS retention_runs_started_idx ON retention_runs (started_at);
CREATE INDEX IF NOT EXISTS retention_runs_period_idx ON retention_runs (period_id);

-- A rule within a run: its cutoff and counts. matched: rows due at the
-- cutoff (an audit: rows past their job's window); changed: rows the rule
-- changed (apply only); related_changed: rows of another table changed with
-- them (guest_identity: the guest's certificate e-mails); overdue: rows still
-- due a day past the cutoff when the rule ended; anchorless: rows the rule
-- cannot date (no Event end), never changed.
CREATE TABLE IF NOT EXISTS retention_run_rules (
    run_id UUID NOT NULL REFERENCES retention_runs (id),
    rule TEXT NOT NULL,
    rule_version INTEGER NOT NULL,
    kind TEXT NOT NULL,
    action TEXT NOT NULL,
    target_table TEXT NOT NULL,
    cutoff TIMESTAMPTZ,
    matched BIGINT NOT NULL DEFAULT 0,
    changed BIGINT NOT NULL DEFAULT 0,
    related_changed BIGINT NOT NULL DEFAULT 0,
    overdue BIGINT NOT NULL DEFAULT 0,
    anchorless BIGINT NOT NULL DEFAULT 0,
    table_rows BIGINT,
    status TEXT NOT NULL,
    error_code TEXT NOT NULL DEFAULT '',
    started_at TIMESTAMPTZ NOT NULL,
    finished_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (run_id, rule),
    CONSTRAINT retention_run_rules_rule_check CHECK (rule ~ '^[a-z][a-z0-9_]{0,63}$'),
    CONSTRAINT retention_run_rules_table_check CHECK (target_table ~ '^[a-z][a-z0-9_]{0,63}$'),
    CONSTRAINT retention_run_rules_version_check CHECK (rule_version > 0),
    CONSTRAINT retention_run_rules_kind_check CHECK (kind IN ('sweep', 'audit')),
    CONSTRAINT retention_run_rules_action_check CHECK (action IN ('scrub', 'delete', 'count')),
    CONSTRAINT retention_run_rules_status_check CHECK (
        status IN ('ok', 'dry_run', 'would_refuse_large', 'refused_large', 'failed', 'not_applicable')
    ),
    CONSTRAINT retention_run_rules_error_code_check CHECK (error_code ~ '^[a-z0-9_]{0,64}$'),
    CONSTRAINT retention_run_rules_counts_check CHECK (
        matched >= 0 AND changed >= 0 AND related_changed >= 0 AND overdue >= 0 AND anchorless >= 0
        AND (table_rows IS NULL OR table_rows >= 0)
    )
);

COMMENT ON TABLE retention_periods IS
    'Periodic destruction periods (docs/retention-sweep.md). No personal data. Kept at least three years; no code path deletes them.';
COMMENT ON TABLE retention_runs IS
    'Periodic destruction runs (docs/retention-sweep.md). No personal data. Kept at least three years; no code path deletes them.';
COMMENT ON TABLE retention_run_rules IS
    'Per-rule counts of a periodic destruction run (docs/retention-sweep.md). No personal data. Kept at least three years; no code path deletes them.';
