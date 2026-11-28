SET LOCAL lock_timeout = '5s';

-- Rolling back drops who wrote a Check-in and the door's taken-back
-- marks: the release before neither reads nor writes them, and its account
-- erasure would not clear the door staff named there. The Check-ins and
-- Venue entries themselves stay. Run it with the code rollback.
DROP TABLE IF EXISTS door_undos;
DROP TRIGGER IF EXISTS ticket_checkins_require_active_writer ON ticket_checkins;
DROP INDEX IF EXISTS ticket_checkins_by_user_idx;
ALTER TABLE ticket_checkins DROP CONSTRAINT IF EXISTS ticket_checkins_source_check;
ALTER TABLE ticket_checkins DROP COLUMN IF EXISTS by_user_id;
ALTER TABLE ticket_checkins DROP COLUMN IF EXISTS source;
