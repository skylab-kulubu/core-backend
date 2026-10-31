-- Management membership terms (docs/management-membership-terms.md;
-- grup-yonetimi 11, GY2, Yusuf 2026-10-12 22:15): a membership of YK, DK
-- (a non-ADMIN Privileged root or a Group under one) or of a Team's
-- LIDERLER/KOORDINATORLER ends with the season. The end is only marked:
-- core warns 14 days and 1 day before, writes expired_at when it passes and
-- tells ADMIN; it never takes anyone out of a Group by itself. ADMIN and
-- ordinary Team memberships have no term.
--
-- Ids only (a person, a Group, a season, who granted it); names, addresses
-- and the Group's name are read when a mail is sent or a list is shown.
-- Account erasure deletes the person's terms and forgets them as the one
-- who granted one (anonymize_core).
CREATE TABLE IF NOT EXISTS management_membership_terms (
    id            UUID        PRIMARY KEY,
    user_id       UUID        NOT NULL,
    group_id      UUID        NOT NULL,
    group_path    TEXT        NOT NULL,
    scope         TEXT        NOT NULL,
    ends_at       TIMESTAMPTZ,
    season_id     UUID,
    granted_by    UUID,
    source        TEXT        NOT NULL,
    started_at    TIMESTAMPTZ NOT NULL,
    warned_14d_at TIMESTAMPTZ,
    warned_1d_at  TIMESTAMPTZ,
    expired_at    TIMESTAMPTZ,
    ended_at      TIMESTAMPTZ,
    updated_at    TIMESTAMPTZ NOT NULL,
    CONSTRAINT management_membership_terms_scope_check CHECK (scope IN ('privileged', 'leader')),
    CONSTRAINT management_membership_terms_source_check CHECK (source IN ('route', 'backfill', 'handover')),
    CONSTRAINT management_membership_terms_group_path_check CHECK (group_path LIKE '/%' AND char_length(group_path) <= 1024),
    -- Only a dated term expires.
    CONSTRAINT management_membership_terms_expired_check CHECK (expired_at IS NULL OR ends_at IS NOT NULL)
);

COMMENT ON TABLE management_membership_terms IS
    'Management membership terms (docs/management-membership-terms.md): YK, DK and Team Leader memberships end with the season; the end is marked (expired_at), never acted on. Ids only.';
COMMENT ON COLUMN management_membership_terms.expired_at IS
    'When core marked the term expired. The person stays in the Group until someone removes them.';
COMMENT ON COLUMN management_membership_terms.ended_at IS
    'When the membership itself ended (removed through core, or found gone from Keycloak).';

-- One open term per person and Group.
CREATE UNIQUE INDEX IF NOT EXISTS management_membership_terms_open_idx
    ON management_membership_terms (user_id, group_id) WHERE ended_at IS NULL;
CREATE INDEX IF NOT EXISTS management_membership_terms_ends_idx
    ON management_membership_terms (ends_at) WHERE ended_at IS NULL;
CREATE INDEX IF NOT EXISTS management_membership_terms_granted_idx
    ON management_membership_terms (granted_by) WHERE granted_by IS NOT NULL;

-- The term mails waiting for SkyMail: one row per recipient, sent like the
-- membership mail's rows (leased, retried with backoff, given up after 72
-- hours, deleted once sent, skipped or refused). A table of its own: an
-- older core never reads it.
CREATE TABLE IF NOT EXISTS management_term_mails (
    id              UUID        PRIMARY KEY,
    recipient_id    UUID        NOT NULL,
    notice          TEXT        NOT NULL,
    term_id         UUID,
    subject_id      UUID,
    group_id        UUID,
    group_path      TEXT        NOT NULL DEFAULT '',
    ends_at         TIMESTAMPTZ,
    term_ids        UUID[]      NOT NULL DEFAULT '{}',
    summary         JSONB,
    occurred_at     TIMESTAMPTZ NOT NULL,
    next_attempt_at TIMESTAMPTZ NOT NULL,
    attempts        INTEGER     NOT NULL DEFAULT 0,
    claimed_at      TIMESTAMPTZ,
    CONSTRAINT management_term_mails_notice_check CHECK (notice IN ('warning_14d', 'warning_1d', 'expired_list', 'weekly_summary')),
    CONSTRAINT management_term_mails_group_path_check CHECK (group_path = '' OR (group_path LIKE '/%' AND char_length(group_path) <= 1024)),
    CONSTRAINT management_term_mails_term_ids_check CHECK (cardinality(term_ids) <= 1000),
    CONSTRAINT management_term_mails_summary_check CHECK (summary IS NULL OR (jsonb_typeof(summary) = 'array' AND octet_length(summary::text) <= 65536)),
    CONSTRAINT management_term_mails_attempts_check CHECK (attempts >= 0)
);

COMMENT ON TABLE management_term_mails IS
    'Management term mails waiting for SkyMail (docs/management-membership-terms.md). Ids, Group paths and counts, no address or name; rows live until sent, at most 72 hours.';

CREATE INDEX IF NOT EXISTS management_term_mails_due_idx ON management_term_mails (next_attempt_at);
CREATE INDEX IF NOT EXISTS management_term_mails_recipient_idx ON management_term_mails (recipient_id);
CREATE INDEX IF NOT EXISTS management_term_mails_subject_idx ON management_term_mails (subject_id) WHERE subject_id IS NOT NULL;

-- One row per run of the daily job: its mode, its counts and how it ended.
-- No person, Group or path.
CREATE TABLE IF NOT EXISTS management_term_runs (
    id            UUID        PRIMARY KEY,
    mode          TEXT        NOT NULL,
    triggered_by  TEXT        NOT NULL,
    started_at    TIMESTAMPTZ NOT NULL,
    finished_at   TIMESTAMPTZ,
    status        TEXT        NOT NULL,
    error_code    TEXT,
    season_found  BOOLEAN     NOT NULL DEFAULT false,
    backfilled    INTEGER     NOT NULL DEFAULT 0,
    dated         INTEGER     NOT NULL DEFAULT 0,
    ended         INTEGER     NOT NULL DEFAULT 0,
    warned_14d    INTEGER     NOT NULL DEFAULT 0,
    warned_1d     INTEGER     NOT NULL DEFAULT 0,
    expired       INTEGER     NOT NULL DEFAULT 0,
    mails         INTEGER     NOT NULL DEFAULT 0,
    weekly        TEXT        NOT NULL DEFAULT 'not_due',
    CONSTRAINT management_term_runs_mode_check CHECK (mode IN ('dry-run', 'apply')),
    CONSTRAINT management_term_runs_trigger_check CHECK (triggered_by IN ('schedule', 'cli')),
    CONSTRAINT management_term_runs_status_check CHECK (status IN ('running', 'ok', 'failed', 'abandoned')),
    CONSTRAINT management_term_runs_error_code_check CHECK (error_code IS NULL OR error_code ~ '^[a-z0-9_]{1,64}$'),
    CONSTRAINT management_term_runs_weekly_check CHECK (weekly IN ('sent', 'none', 'not_due', 'off'))
);

COMMENT ON TABLE management_term_runs IS
    'Runs of the management term job (docs/management-membership-terms.md): counts only.';

CREATE INDEX IF NOT EXISTS management_term_runs_started_idx ON management_term_runs (started_at DESC);
