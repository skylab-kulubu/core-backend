-- Rolling back drops the ticket note: the release before reads none.
SET LOCAL lock_timeout = '5s';

ALTER TABLE events DROP CONSTRAINT IF EXISTS events_ticket_note_check;
ALTER TABLE events DROP COLUMN IF EXISTS ticket_note;
