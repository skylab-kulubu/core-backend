-- Pulling a form's accepted answers from Forms (docs/form-pulls.md,
-- rabbitmq-olaylar 18; SPEC §10 eki): core's own work queue (ADR-0071
-- karar 9: in-service queues stay in PostgreSQL).
--
--   * form_pulls: one job a row, deleted when done or given up (7 days).
--     kind backfill is an organizer's "Bu kişilere de bilet yaz" (every
--     accepted answer, event_id and requested_by say for whom and by whom);
--     catchup is the daily pass (the answers changed since changed_since).
--     At most one job of each kind a form. cursor is the list's next page
--     while the job runs. Ids and times only.
--   * form_pull_watermarks: one row a form the daily pass knows. watermark
--     is Forms' time up to which core has the form's answers (the first
--     time the pass saw the form linked, else the last pull's), and the
--     next pass asks from watermark − 5 minutes; next_catchup_at is when.
--     last_backfill_at is when an organizer last asked for the form's
--     backfill: a second ask within 15 minutes is refused (429). A row
--     whose form no current Event lists goes after 30 days.
--
-- Expand only: nothing reads them before this release.
SET LOCAL lock_timeout = '5s';

CREATE TABLE IF NOT EXISTS form_pulls (
    id UUID PRIMARY KEY,
    form_id UUID NOT NULL,
    kind TEXT NOT NULL CHECK (kind IN ('backfill', 'catchup')),
    event_id UUID,
    requested_by UUID,
    requested_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    changed_since TIMESTAMPTZ,
    cursor TEXT CHECK (cursor IS NULL OR length(cursor) <= 2048),
    attempts INTEGER NOT NULL DEFAULT 0,
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT form_pulls_shape_check CHECK (
        (kind = 'backfill' AND event_id IS NOT NULL AND changed_since IS NULL)
        OR (kind = 'catchup' AND event_id IS NULL AND requested_by IS NULL AND changed_since IS NOT NULL))
);

CREATE UNIQUE INDEX IF NOT EXISTS form_pulls_form_kind_idx ON form_pulls (form_id, kind);
CREATE INDEX IF NOT EXISTS form_pulls_due_idx ON form_pulls (next_attempt_at);

CREATE TABLE IF NOT EXISTS form_pull_watermarks (
    form_id UUID PRIMARY KEY,
    watermark TIMESTAMPTZ NOT NULL,
    next_catchup_at TIMESTAMPTZ NOT NULL,
    last_backfill_at TIMESTAMPTZ,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
