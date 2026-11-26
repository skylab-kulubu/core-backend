SET LOCAL lock_timeout = '5s';

-- A new Event's switch is on by default again. Only the column's default
-- changes; every Event keeps its value.
ALTER TABLE events ALTER COLUMN attendance_auto_finalize SET DEFAULT true;
