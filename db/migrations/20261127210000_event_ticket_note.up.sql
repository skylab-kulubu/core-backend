-- An Event's ticket note (docs/ticket-note.md): the organizer's short
-- announcement for the Ticket holders, the event's WhatsApp group say,
-- plain text up to 500 characters, empty for none. The ticket mails carry
-- it (event.ticket-created, event.waitlist-promoted, event.reminder), its
-- https addresses as links. Only who reads the attendee note reads it in
-- an answer: never a public one.
--
-- The release before runs on the new schema: it neither reads nor writes
-- the column (it has a default).
SET LOCAL lock_timeout = '5s';

ALTER TABLE events ADD COLUMN IF NOT EXISTS ticket_note TEXT NOT NULL DEFAULT '';
ALTER TABLE events DROP CONSTRAINT IF EXISTS events_ticket_note_check;
ALTER TABLE events ADD CONSTRAINT events_ticket_note_check CHECK (char_length(ticket_note) <= 500);
COMMENT ON COLUMN events.ticket_note IS 'The organizer''s announcement for the Ticket holders (WhatsApp group): the ticket mails carry it (docs/ticket-note.md).';
