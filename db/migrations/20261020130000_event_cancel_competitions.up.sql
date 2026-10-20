-- Cancelling an Event cancels its Competitions with it, and undoing the
-- cancellation brings back those, and only those, to the state they had
-- (ADR 0066 addendum 2026-10-19, decision 2; tickets 04 and 15).
--
--   * cancelled_with_event_at: when the Event's cancellation cancelled it;
--     none for a Competition cancelled on its own.
--   * status_before_event_cancel: its status just before.
--
-- The release before this one keeps working on the new schema: its writes
-- name their columns and take the defaults.
SET LOCAL lock_timeout = '5s';

ALTER TABLE competitions
    ADD COLUMN IF NOT EXISTS cancelled_with_event_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS status_before_event_cancel TEXT;

ALTER TABLE competitions
    DROP CONSTRAINT IF EXISTS competitions_status_before_event_cancel_check,
    ADD CONSTRAINT competitions_status_before_event_cancel_check CHECK (
        (cancelled_with_event_at IS NULL AND status_before_event_cancel IS NULL)
        OR (cancelled_with_event_at IS NOT NULL
            AND status_before_event_cancel IS NOT NULL
            AND status_before_event_cancel IN ('draft', 'open', 'running', 'closed'))
    );
