SET LOCAL lock_timeout = '5s';

-- Rolling back drops the devices' scan moments; the rows stay. The release
-- before neither writes nor reads the column, so rolling back the code
-- alone needs no down migration.
ALTER TABLE venue_entries DROP COLUMN IF EXISTS scanned_at;
ALTER TABLE ticket_checkins DROP COLUMN IF EXISTS scanned_at;
