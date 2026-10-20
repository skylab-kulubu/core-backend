-- Rolls back registration closure: applying is open again on every Event,
-- as the release before this one knows it.
ALTER TABLE events
    DROP COLUMN IF EXISTS registration_closes_at,
    DROP COLUMN IF EXISTS registration_closed;
