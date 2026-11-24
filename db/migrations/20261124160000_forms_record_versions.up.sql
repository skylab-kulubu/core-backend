-- core.forms' version order (docs/integration-events.md "core.forms"): the
-- latest version core handled of each Forms record an Integration event
-- named, a response or a form. An event of an older or equal version changes
-- nothing (ADR-0071 §5: the order of events is not guaranteed). Ids and a
-- number only, no personal data; a row goes 30 days after its last change,
-- with consumed_events. Expand only: nothing reads it before this release.
SET LOCAL lock_timeout = '5s';

CREATE TABLE IF NOT EXISTS forms_record_versions (
    kind TEXT NOT NULL CHECK (kind IN ('response', 'form')),
    record_id UUID NOT NULL,
    version BIGINT NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (kind, record_id)
);

CREATE INDEX IF NOT EXISTS forms_record_versions_updated_at_idx ON forms_record_versions (updated_at);
