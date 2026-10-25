-- Puts events.active back, as the derived active (status.go's activeSQL) at
-- the time of the rollback, for a release that still reads the column.
SET LOCAL lock_timeout = '5s';

ALTER TABLE events ADD COLUMN IF NOT EXISTS active BOOLEAN NOT NULL DEFAULT false;
UPDATE events e SET active = (e.status = 'published' AND (
    COALESCE(e.end_date, e.start_date + interval '24 hours') IS NULL
    OR COALESCE(e.end_date, e.start_date + interval '24 hours') > now()));
