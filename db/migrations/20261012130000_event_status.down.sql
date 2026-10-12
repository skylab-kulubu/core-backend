-- Rolls back the Event status. events.active was never touched, so the
-- release before this one reads it as it left it; a draft created since was
-- written with active = false and stays hidden there (the safe way round),
-- and a cancelled Event keeps the flag it had.
DROP TRIGGER IF EXISTS events_require_active_canceller ON events;
DROP TRIGGER IF EXISTS events_require_active_publisher ON events;
DROP INDEX IF EXISTS events_current_status_idx;
ALTER TABLE events
    DROP COLUMN IF EXISTS copied_from_event_id,
    DROP COLUMN IF EXISTS cancel_reason,
    DROP COLUMN IF EXISTS cancelled_by,
    DROP COLUMN IF EXISTS cancelled_at,
    DROP COLUMN IF EXISTS published_by,
    DROP COLUMN IF EXISTS published_at,
    DROP COLUMN IF EXISTS status;
