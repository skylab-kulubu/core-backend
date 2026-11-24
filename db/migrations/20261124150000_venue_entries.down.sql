-- Rolling back drops the door's mode and every Venue entry: the release
-- before reads neither, and a Check-in is kept (attendance does not depend
-- on the door).
DROP TABLE IF EXISTS venue_entries;
ALTER TABLE events DROP CONSTRAINT IF EXISTS events_entry_mode_check;
ALTER TABLE events DROP COLUMN IF EXISTS entry_mode;
