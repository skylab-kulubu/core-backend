-- Consumed Integration events (ADR-0071, docs/integration-events.md): one
-- row per event a consumer of core's has handled, written in the same
-- transaction as the handler's own writes. A repeat delivery of the same
-- (source, id) finds the row and has no second effect (CloudEvents: source
-- and id together are unique).
--
--   * consumer: the consumer's name, its queue by default (core.forms).
--   * source, event_id: the CloudEvents source and id (for Keycloak's own
--     format, what its decoder takes for them).
--   * processed_at: when it was handled. A row goes after 30 days (the
--     hourly cleanup of internal/events): longer than a publisher's outbox
--     (7 days) and a DLQ (14 days) keep a copy.
--
-- Ids only, no personal data. Expand only: nothing reads it before this
-- release.
SET LOCAL lock_timeout = '5s';

CREATE TABLE IF NOT EXISTS consumed_events (
    consumer TEXT NOT NULL CHECK (length(consumer) BETWEEN 1 AND 100),
    source TEXT NOT NULL CHECK (length(source) BETWEEN 1 AND 255),
    event_id TEXT NOT NULL CHECK (length(event_id) BETWEEN 1 AND 255),
    processed_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT consumed_events_pkey PRIMARY KEY (consumer, source, event_id)
);

CREATE INDEX IF NOT EXISTS consumed_events_processed_at_idx ON consumed_events (processed_at);
