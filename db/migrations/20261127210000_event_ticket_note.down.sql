SET LOCAL lock_timeout = '5s';

-- Rolling back keeps events.ticket_note with its values and check: the
-- release before neither reads nor writes it (it has a default), and the
-- next release reads it again (the up migration finds it, ADD COLUMN IF NOT
-- EXISTS). To remove it for good, by hand: ALTER TABLE events DROP
-- CONSTRAINT events_ticket_note_check, DROP COLUMN ticket_note;
SELECT 1;
