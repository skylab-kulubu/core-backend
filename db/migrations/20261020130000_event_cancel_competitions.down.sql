-- Rolls back the record of the Competitions an Event's cancellation
-- cancelled; they stay cancelled, as the release before knows them.
ALTER TABLE competitions
    DROP CONSTRAINT IF EXISTS competitions_status_before_event_cancel_check,
    DROP COLUMN IF EXISTS status_before_event_cancel,
    DROP COLUMN IF EXISTS cancelled_with_event_at;
